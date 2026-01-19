// Package main implements the GPU pooling central server.
package main

import (
	"fmt"
	"os"

	"github.com/janvanoekelen/metalyard/src/shared"
)

func main() {
	fmt.Println("gpu-server starting...")

	// Placeholder config
	config := shared.ServerConfig{
		ListenAddr:   ":8080",
		DatabasePath: "./gpu-pool.db",
	}

	fmt.Printf("Config: listen=%s db=%s\n", config.ListenAddr, config.DatabasePath)

	// TODO: Initialize SQLite
	// TODO: Start HTTP server
	// TODO: Agent registry
	// TODO: Scheduler

	fmt.Println("gpu-server ready (stub)")
	os.Exit(0)
}
