package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

var (
	configPath = flag.String("config", "", "Path to config file (default: ~/.config/gpu-agent/config.json)")
	logLevel   = flag.String("log-level", "", "Log level: debug, info, warn, error")
)

func main() {
	flag.Parse()

	// Load configuration
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if *logLevel != "" {
		cfg.LogLevel = *logLevel
	}

	log.Printf("GPU Agent starting...")
	log.Printf("Server URL: %s", cfg.ServerURL)
	log.Printf("Log level: %s", cfg.LogLevel)

	// Detect GPUs
	gpus, err := DetectGPUs()
	if err != nil {
		log.Fatalf("Failed to detect GPUs: %v", err)
	}

	if len(gpus) == 0 {
		log.Fatalf("No GPUs detected")
	}

	// Use first GPU (single GPU per agent for now)
	gpu := gpus[0]
	log.Printf("Detected GPU: %s", gpu.String())

	// Create heartbeat client
	hbClient := NewHeartbeatClient(cfg.ServerURL, cfg.APIKey)

	// Register with server
	log.Printf("Registering with server...")
	regResp, err := hbClient.Register(gpu)
	if err != nil {
		log.Fatalf("Failed to register: %v", err)
	}

	agentID := regResp.AgentID
	log.Printf("Registered successfully. Agent ID: %s", agentID)
	log.Printf("Available models: %s", strings.Join(regResp.Models, ", "))

	// Save agent ID to config
	cfg.AgentID = agentID
	if err := SaveConfig(cfg, *configPath); err != nil {
		log.Printf("Warning: failed to save config with agent ID: %v", err)
	}

	// Setup graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		log.Printf("Received signal %v, shutting down...", sig)
		cancel()
	}()

	// Track uptime
	startTime := time.Now()

	// Main heartbeat loop
	ticker := time.NewTicker(HeartbeatInterval)
	defer ticker.Stop()

	log.Printf("Starting heartbeat loop (interval: %v)", HeartbeatInterval)

	// Send initial heartbeat immediately
	sendHeartbeat(hbClient, agentID, gpu, startTime)

	for {
		select {
		case <-ctx.Done():
			log.Printf("Shutdown complete")
			return

		case <-ticker.C:
			sendHeartbeat(hbClient, agentID, gpu, startTime)
		}
	}
}

func sendHeartbeat(client *HeartbeatClient, agentID string, gpu GPUInfo, startTime time.Time) {
	hb := Heartbeat{
		AgentID:      agentID,
		Status:       "available", // TODO: track actual status
		LoadedModel:  "",          // TODO: track loaded model
		TemperatureC: 0,           // TODO: read GPU temperature
		UptimeSec:    int(time.Since(startTime).Seconds()),
		GPU:          gpu,
	}

	resp, err := client.SendHeartbeat(hb)
	if err != nil {
		log.Printf("Heartbeat failed: %v", err)
		return
	}

	if !resp.Ack {
		log.Printf("Heartbeat not acknowledged")
		return
	}

	log.Printf("Heartbeat sent (uptime: %ds)", hb.UptimeSec)

	// Handle commands from server
	for _, cmd := range resp.Commands {
		handleCommand(cmd)
	}
}

func handleCommand(cmd string) {
	log.Printf("Received command: %s", cmd)

	switch {
	case strings.HasPrefix(cmd, "load_model:"):
		model := strings.TrimPrefix(cmd, "load_model:")
		log.Printf("Server requested model load: %s", model)
		// TODO: implement model loading

	case cmd == "shutdown":
		log.Printf("Server requested shutdown")
		os.Exit(0)

	default:
		log.Printf("Unknown command: %s", cmd)
	}
}
