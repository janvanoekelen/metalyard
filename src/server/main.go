// Package main implements the GPU pooling central server.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Config holds the server configuration
type Config struct {
	Addr              string
	DBPath            string
	AdminAPIKey       string
	HeartbeatInterval int           // seconds
	StaleTimeout      time.Duration // how long before agent is marked offline
	CleanupInterval   time.Duration // how often to check for stale agents
}

func main() {
	// Parse flags
	config := Config{}
	flag.StringVar(&config.Addr, "addr", ":8080", "Server listen address")
	flag.StringVar(&config.DBPath, "db", "gpupool.db", "SQLite database path")
	flag.StringVar(&config.AdminAPIKey, "admin-key", "", "Admin API key (required)")
	flag.IntVar(&config.HeartbeatInterval, "heartbeat-interval", 30, "Heartbeat interval in seconds")
	flag.DurationVar(&config.StaleTimeout, "stale-timeout", 90*time.Second, "Time before agent is marked offline")
	flag.DurationVar(&config.CleanupInterval, "cleanup-interval", 30*time.Second, "Stale agent cleanup interval")
	flag.Parse()

	// Allow env var override
	if envKey := os.Getenv("GPUPOOL_ADMIN_KEY"); envKey != "" {
		config.AdminAPIKey = envKey
	}
	if envAddr := os.Getenv("GPUPOOL_ADDR"); envAddr != "" {
		config.Addr = envAddr
	}
	if envDB := os.Getenv("GPUPOOL_DB"); envDB != "" {
		config.DBPath = envDB
	}

	// Validate config
	if config.AdminAPIKey == "" {
		log.Fatal("Admin API key is required (use -admin-key or GPUPOOL_ADMIN_KEY)")
	}

	// Open database
	db, err := OpenDB(config.DBPath)
	if err != nil {
		log.Fatalf("Failed to open database: %v", err)
	}
	defer db.Close()

	// Create handlers
	handlers := NewHandlers(db, config.AdminAPIKey, config.HeartbeatInterval)

	// Set up routes
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agents/register", handlers.HandleRegister)
	mux.HandleFunc("/v1/agents/", handlers.HandleHeartbeat) // Matches /v1/agents/{id}/heartbeat
	mux.HandleFunc("/v1/admin/agents", handlers.HandleAdminAgents)
	mux.HandleFunc("/health", handlers.HandleHealth)

	// Create server
	server := &http.Server{
		Addr:         config.Addr,
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start scheduler
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	scheduler := NewScheduler(db, config.StaleTimeout, config.CleanupInterval)
	go scheduler.Run(ctx)

	// Handle shutdown
	done := make(chan bool)
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
		<-sigChan

		log.Println("Shutting down...")
		cancel() // Stop scheduler

		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("Error during shutdown: %v", err)
		}
		done <- true
	}()

	// Start server
	log.Printf("Server starting on %s", config.Addr)
	log.Printf("Database: %s", config.DBPath)
	log.Printf("Heartbeat interval: %ds, stale timeout: %v", config.HeartbeatInterval, config.StaleTimeout)

	if err := server.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("Server error: %v", err)
	}

	<-done
	log.Println("Server stopped")
}
