package shared

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// API endpoint paths
const (
	PathAgentRegister  = "/api/v1/agents/register"
	PathAgentHeartbeat = "/api/v1/agents/%s/heartbeat" // %s = agent_id
	PathAgentWork      = "/api/v1/agents/%s/work"      // %s = agent_id
	PathAgentResult    = "/api/v1/agents/%s/result"    // %s = agent_id
	PathCompletions    = "/v1/completions"
)

// Error types for the protocol.
var (
	ErrNoCapableAgents   = &ProtocolError{Code: "NO_CAPABLE_AGENTS", Message: "no agents capable of running the requested model"}
	ErrNoAvailableAgents = &ProtocolError{Code: "NO_AVAILABLE_AGENTS", Message: "all capable agents are currently busy"}
	ErrAgentOffline      = &ProtocolError{Code: "AGENT_OFFLINE", Message: "agent is offline"}
	ErrModelNotFound     = &ProtocolError{Code: "MODEL_NOT_FOUND", Message: "requested model not found"}
	ErrUnauthorized      = &ProtocolError{Code: "UNAUTHORIZED", Message: "invalid or missing API key"}
	ErrTimeout           = &ProtocolError{Code: "TIMEOUT", Message: "request timed out"}
	ErrInternalServer    = &ProtocolError{Code: "INTERNAL_ERROR", Message: "internal server error"}
)

// ProtocolError represents an error in the GPU pooling protocol.
type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

func (e *ProtocolError) Error() string {
	if e.Details != "" {
		return fmt.Sprintf("%s: %s (%s)", e.Code, e.Message, e.Details)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// WithDetails returns a copy of the error with additional details.
func (e *ProtocolError) WithDetails(details string) *ProtocolError {
	return &ProtocolError{
		Code:    e.Code,
		Message: e.Message,
		Details: details,
	}
}

// ErrorResponse is the standard error response format.
type ErrorResponse struct {
	Error ProtocolError `json:"error"`
}

// Client provides HTTP client helpers for the protocol.
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// NewClient creates a new protocol client.
func NewClient(baseURL, apiKey string) *Client {
	return &Client{
		BaseURL: baseURL,
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Do performs an HTTP request with JSON encoding/decoding.
func (c *Client) Do(ctx context.Context, method, path string, body, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("performing request: %w", err)
	}
	defer resp.Body.Close()

	// Handle error responses
	if resp.StatusCode >= 400 {
		var errResp ErrorResponse
		if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
			return &ProtocolError{
				Code:    "HTTP_ERROR",
				Message: fmt.Sprintf("HTTP %d", resp.StatusCode),
			}
		}
		return &errResp.Error
	}

	// Handle 204 No Content
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}

	// Decode successful response
	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decoding response: %w", err)
		}
	}

	return nil
}

// Get performs an HTTP GET request.
func (c *Client) Get(ctx context.Context, path string, result any) error {
	return c.Do(ctx, http.MethodGet, path, nil, result)
}

// Post performs an HTTP POST request.
func (c *Client) Post(ctx context.Context, path string, body, result any) error {
	return c.Do(ctx, http.MethodPost, path, body, result)
}

// WriteJSON writes a JSON response.
func WriteJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// WriteError writes an error response.
func WriteError(w http.ResponseWriter, status int, err *ProtocolError) {
	WriteJSON(w, status, ErrorResponse{Error: *err})
}

// ParseJSON parses a JSON request body.
func ParseJSON[T any](r *http.Request) (*T, error) {
	var data T
	if err := json.NewDecoder(r.Body).Decode(&data); err != nil {
		return nil, fmt.Errorf("parsing request body: %w", err)
	}
	return &data, nil
}

// SetSSEHeaders sets headers for Server-Sent Events streaming.
func SetSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// WriteSSEEvent writes a single SSE event.
func WriteSSEEvent(w http.ResponseWriter, data any) error {
	encoded, err := json.Marshal(data)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "data: %s\n\n", encoded)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

// WriteSSEDone writes the final [DONE] marker for SSE streams.
func WriteSSEDone(w http.ResponseWriter) {
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
