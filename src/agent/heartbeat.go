package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// HeartbeatClient handles registration and heartbeat communication with server
type HeartbeatClient struct {
	serverURL  string
	apiKey     string
	httpClient *http.Client
}

// NewHeartbeatClient creates a new heartbeat client
func NewHeartbeatClient(serverURL, apiKey string) *HeartbeatClient {
	return &HeartbeatClient{
		serverURL: serverURL,
		apiKey:    apiKey,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// RegistrationRequest is sent when agent first registers
type RegistrationRequest struct {
	GPU GPUInfo `json:"gpu"`
}

// RegistrationResponse is returned by server on registration
type RegistrationResponse struct {
	AgentID    string   `json:"agent_id"`
	ServerTime int64    `json:"server_time"`
	Models     []string `json:"models"`
}

// Heartbeat represents the periodic health report
type Heartbeat struct {
	AgentID      string  `json:"agent_id"`
	Status       string  `json:"status"`        // "available", "busy", "degraded"
	LoadedModel  string  `json:"loaded_model"`  // Empty if none
	TemperatureC int     `json:"temperature_c"` // GPU temp if available
	UptimeSec    int     `json:"uptime_sec"`
	GPU          GPUInfo `json:"gpu"`
}

// HeartbeatResponse is returned by server on heartbeat
type HeartbeatResponse struct {
	Ack      bool     `json:"ack"`
	Commands []string `json:"commands"` // e.g., ["load_model:llama-7b-q4"], ["shutdown"]
}

// Register registers the agent with the server
func (c *HeartbeatClient) Register(gpu GPUInfo) (*RegistrationResponse, error) {
	url := fmt.Sprintf("%s/api/v1/agents/register", c.serverURL)

	reqBody := RegistrationRequest{GPU: gpu}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registration failed: %s - %s", resp.Status, string(respBody))
	}

	var regResp RegistrationResponse
	if err := json.NewDecoder(resp.Body).Decode(&regResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &regResp, nil
}

// SendHeartbeat sends a heartbeat to the server
func (c *HeartbeatClient) SendHeartbeat(hb Heartbeat) (*HeartbeatResponse, error) {
	url := fmt.Sprintf("%s/api/v1/agents/%s/heartbeat", c.serverURL, hb.AgentID)

	body, err := json.Marshal(hb)
	if err != nil {
		return nil, fmt.Errorf("marshaling heartbeat: %w", err)
	}

	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", c.apiKey))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("sending heartbeat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("heartbeat failed: %s - %s", resp.Status, string(respBody))
	}

	var hbResp HeartbeatResponse
	if err := json.NewDecoder(resp.Body).Decode(&hbResp); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	return &hbResp, nil
}

// HeartbeatInterval is the time between heartbeats
const HeartbeatInterval = 30 * time.Second

// MaxMissedHeartbeats before agent is considered offline
const MaxMissedHeartbeats = 3
