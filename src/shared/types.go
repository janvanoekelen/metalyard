// Package shared contains types and utilities used by both the GPU pooling
// agent and server components.
package shared

import "time"

// GPUInfo describes a GPU's capabilities for scheduling purposes.
type GPUInfo struct {
	ID         string `json:"id"`          // e.g., "cuda:0", "metal:0", "cpu"
	Type       string `json:"type"`        // "nvidia", "apple", "cpu"
	Name       string `json:"name"`        // e.g., "RTX 4090", "M2 Max"
	VRAM_MB    int    `json:"vram_mb"`     // Available VRAM (0 for unified memory)
	ComputeCap string `json:"compute_cap"` // e.g., "8.9" for NVIDIA, "apple3" for Metal
}

// CanServe checks if the GPU can serve a model requiring the given VRAM.
func (g GPUInfo) CanServe(modelVRAM_MB int) bool {
	if g.Type == "cpu" {
		return true // CPU always works (slowly)
	}
	if g.VRAM_MB < modelVRAM_MB+512 { // 512MB headroom
		return false
	}
	if g.Type == "nvidia" && g.ComputeCap < "7.0" {
		return false // Pre-Volta too slow
	}
	return true
}

// Agent represents a registered GPU agent in the system.
type Agent struct {
	ID            string    `json:"id"`
	APIKeyHash    string    `json:"-"` // Not exposed in JSON
	GPU           GPUInfo   `json:"gpu"`
	Status        string    `json:"status"`       // "online", "busy", "degraded", "offline"
	LoadedModel   string    `json:"loaded_model"` // empty if none
	LastHeartbeat time.Time `json:"last_heartbeat"`
	Reliability   float64   `json:"reliability"` // 0.0-1.0
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// RegistrationRequest is sent by agents when registering with the server.
type RegistrationRequest struct {
	GPU GPUInfo `json:"gpu"`
}

// RegistrationResponse is returned by the server after successful registration.
type RegistrationResponse struct {
	AgentID    string   `json:"agent_id"`
	ServerTime int64    `json:"server_time"` // Unix timestamp
	Models     []string `json:"models"`      // Available model list
}

// HeartbeatRequest is sent periodically by agents to report their status.
type HeartbeatRequest struct {
	Status       string `json:"status"`        // "online", "busy", "degraded"
	LoadedModel  string `json:"loaded_model"`  // empty if none
	TemperatureC int    `json:"temperature_c"` // GPU temperature
	UptimeSec    int    `json:"uptime_sec"`    // Agent uptime
}

// HeartbeatResponse is returned by the server acknowledging the heartbeat.
type HeartbeatResponse struct {
	Ack      bool     `json:"ack"`
	Commands []string `json:"commands"` // e.g., ["load_model:mistral-7b-q4"], ["shutdown"]
}

// WorkResponse is returned when an agent polls for work.
type WorkResponse struct {
	RequestID string `json:"request_id"`
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
}

// ResultRequest is sent by agents when submitting inference results.
type ResultRequest struct {
	RequestID string   `json:"request_id"`
	Tokens    []string `json:"tokens"`
	Finished  bool     `json:"finished"`
	Error     *string  `json:"error"` // nil if no error
}

// ResultResponse acknowledges receipt of inference results.
type ResultResponse struct {
	Ack bool `json:"ack"`
}

// CompletionRequest is sent by clients requesting inference.
type CompletionRequest struct {
	Model     string `json:"model"`
	Prompt    string `json:"prompt"`
	MaxTokens int    `json:"max_tokens"`
	Stream    bool   `json:"stream"`
}

// CompletionChoice represents a single completion result.
type CompletionChoice struct {
	Text         string `json:"text"`
	FinishReason string `json:"finish_reason"` // "stop", "length"
}

// CompletionUsage tracks token usage for a completion.
type CompletionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// CompletionResponse is returned to clients after inference completes.
type CompletionResponse struct {
	ID      string             `json:"id"`
	Model   string             `json:"model"`
	Choices []CompletionChoice `json:"choices"`
	Usage   CompletionUsage    `json:"usage"`
}

// StreamChunk represents a single chunk in a streaming response.
type StreamChunk struct {
	Choices []CompletionChoice `json:"choices"`
}
