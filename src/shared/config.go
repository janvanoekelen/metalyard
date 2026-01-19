package shared

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// AgentConfig holds configuration for the GPU agent.
type AgentConfig struct {
	ServerURL         string        `json:"server_url" yaml:"server_url"`
	APIKey            string        `json:"api_key" yaml:"api_key"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval" yaml:"heartbeat_interval"`
	ModelCacheDir     string        `json:"model_cache_dir" yaml:"model_cache_dir"`
	LlamaServerPath   string        `json:"llama_server_path" yaml:"llama_server_path"`
	LocalPort         int           `json:"local_port" yaml:"local_port"`
}

// ServerConfig holds configuration for the GPU pooling server.
type ServerConfig struct {
	ListenAddr          string        `json:"listen_addr" yaml:"listen_addr"`
	DatabasePath        string        `json:"database_path" yaml:"database_path"`
	StaleAgentThreshold time.Duration `json:"stale_agent_threshold" yaml:"stale_agent_threshold"`
	RequestTimeout      time.Duration `json:"request_timeout" yaml:"request_timeout"`
	ModelRegistry       []ModelConfig `json:"models" yaml:"models"`
}

// ModelConfig describes a model available in the system.
type ModelConfig struct {
	Name          string `json:"name" yaml:"name"`
	VRAMRequired  int    `json:"vram_required_mb" yaml:"vram_required_mb"`
	MinComputeCap string `json:"min_compute_cap" yaml:"min_compute_cap"`
	DownloadURL   string `json:"download_url" yaml:"download_url"`
}

// LoadConfig reads configuration from a file (JSON or YAML based on extension).
func LoadConfig[T any](path string) (*T, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var config T
	ext := filepath.Ext(path)

	switch ext {
	case ".json":
		if err := json.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("parsing JSON config: %w", err)
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(data, &config); err != nil {
			return nil, fmt.Errorf("parsing YAML config: %w", err)
		}
	default:
		return nil, fmt.Errorf("unsupported config file extension: %s (use .json, .yaml, or .yml)", ext)
	}

	return &config, nil
}

// LoadAgentConfig loads agent configuration from a file.
func LoadAgentConfig(path string) (*AgentConfig, error) {
	config, err := LoadConfig[AgentConfig](path)
	if err != nil {
		return nil, err
	}

	// Apply defaults
	if config.HeartbeatInterval == 0 {
		config.HeartbeatInterval = 30 * time.Second
	}
	if config.LocalPort == 0 {
		config.LocalPort = 8081
	}
	if config.LlamaServerPath == "" {
		config.LlamaServerPath = "llama-server"
	}

	return config, nil
}

// LoadServerConfig loads server configuration from a file.
func LoadServerConfig(path string) (*ServerConfig, error) {
	config, err := LoadConfig[ServerConfig](path)
	if err != nil {
		return nil, err
	}

	// Apply defaults
	if config.ListenAddr == "" {
		config.ListenAddr = ":8080"
	}
	if config.DatabasePath == "" {
		config.DatabasePath = "gpupool.db"
	}
	if config.StaleAgentThreshold == 0 {
		config.StaleAgentThreshold = 90 * time.Second
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = 5 * time.Minute
	}

	return config, nil
}

// SaveConfig writes configuration to a file (JSON or YAML based on extension).
func SaveConfig[T any](path string, config *T) error {
	ext := filepath.Ext(path)
	var data []byte
	var err error

	switch ext {
	case ".json":
		data, err = json.MarshalIndent(config, "", "  ")
	case ".yaml", ".yml":
		data, err = yaml.Marshal(config)
	default:
		return fmt.Errorf("unsupported config file extension: %s (use .json, .yaml, or .yml)", ext)
	}

	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing config file: %w", err)
	}

	return nil
}
