// Package main implements the GPU pooling desktop agent.
package main

import (
	"fmt"
	"os"

	"github.com/janvanoekelen/metalyard/src/shared"
)

func main() {
	fmt.Println("gpu-agent starting...")

	// Placeholder config
	config := shared.AgentConfig{
		ServerURL:    "http://localhost:8080",
		PollInterval: 1000,
		HeartbeatSec: 30,
	}

	fmt.Printf("Config: server=%s poll=%dms heartbeat=%ds\n",
		config.ServerURL, config.PollInterval, config.HeartbeatSec)

	// TODO: GPU detection
	// TODO: Registration
	// TODO: Work polling loop

	fmt.Println("gpu-agent ready (stub)")
	os.Exit(0)
}
