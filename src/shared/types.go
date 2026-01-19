// Package shared contains common types and configuration used by both agent and server.
package shared

// GPUInfo describes a detected GPU's capabilities.
type GPUInfo struct {
	Name       string `json:"name"`
	Vendor     string `json:"vendor"`
	VRAM_MB    int    `json:"vram_mb"`
	ComputeCap string `json:"compute_cap"`
}

// AgentConfig holds configuration for the desktop agent.
type AgentConfig struct {
	ServerURL    string `json:"server_url"`
	AgentID      string `json:"agent_id"`
	ModelsDir    string `json:"models_dir"`
	PollInterval int    `json:"poll_interval_ms"`
	HeartbeatSec int    `json:"heartbeat_sec"`
}

// ServerConfig holds configuration for the central server.
type ServerConfig struct {
	ListenAddr string `json:"listen_addr"`
	DBPath     string `json:"db_path"`
}
