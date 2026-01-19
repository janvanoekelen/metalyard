package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config holds agent configuration
type Config struct {
	ServerURL string `json:"server_url"`
	APIKey    string `json:"api_key"`
	AgentID   string `json:"agent_id,omitempty"` // Assigned by server on first registration
	LogLevel  string `json:"log_level,omitempty"`
}

// DefaultConfigPath returns the default config file path
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "gpu-agent.json"
	}
	return filepath.Join(home, ".config", "gpu-agent", "config.json")
}

// LoadConfig loads configuration from file
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	if cfg.ServerURL == "" {
		return nil, fmt.Errorf("server_url is required")
	}
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("api_key is required")
	}

	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}

	return &cfg, nil
}

// SaveConfig saves configuration to file
func SaveConfig(cfg *Config, path string) error {
	if path == "" {
		path = DefaultConfigPath()
	}

	// Ensure directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	return nil
}
