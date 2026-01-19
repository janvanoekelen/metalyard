package main

import (
	"crypto/subtle"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// RegisterRequest is the request body for agent registration
type RegisterRequest struct {
	Name         string        `json:"name"`
	APIKey       string        `json:"api_key"`
	Capabilities Capabilities  `json:"capabilities"`
	Models       []ModelInfo   `json:"models"`
}

// Capabilities describes the agent's hardware
type Capabilities struct {
	GPUVendor   string `json:"gpu_vendor"`
	GPUModel    string `json:"gpu_model"`
	VRAM_MB     int    `json:"vram_mb"`
	Platform    string `json:"platform"`
}

// ModelInfo describes a model the agent can serve
type ModelInfo struct {
	Name         string `json:"name"`
	Quantization string `json:"quantization,omitempty"`
	MaxContext   int    `json:"max_context"`
}

// RegisterResponse is the response for successful registration
type RegisterResponse struct {
	AgentID          string `json:"agent_id"`
	HeartbeatInterval int    `json:"heartbeat_interval_sec"`
}

// HeartbeatRequest is the request body for heartbeat
type HeartbeatRequest struct {
	Status         string       `json:"status"`
	CurrentLoad    int          `json:"current_load"`
	Capabilities   Capabilities `json:"capabilities"`
}

// HeartbeatResponse is the response for successful heartbeat
type HeartbeatResponse struct {
	Acknowledged bool `json:"acknowledged"`
	NextInterval int  `json:"next_interval_sec"`
}

// AdminAgentInfo is the agent info returned by the admin endpoint
type AdminAgentInfo struct {
	AgentID       string    `json:"agent_id"`
	Name          string    `json:"name"`
	Status        string    `json:"status"`
	LastHeartbeat time.Time `json:"last_heartbeat"`
	CurrentLoad   int       `json:"current_load"`
	Capabilities  Capabilities `json:"capabilities"`
	Models        []ModelInfo  `json:"models"`
}

// AdminResponse is the response for the admin agents endpoint
type AdminResponse struct {
	Agents []AdminAgentInfo `json:"agents"`
	Total  int              `json:"total"`
	Online int              `json:"online"`
}

// ErrorResponse is a standard error response
type ErrorResponse struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

// Handlers holds the HTTP handlers and their dependencies
type Handlers struct {
	db               *DB
	adminAPIKey      string
	heartbeatInterval int
}

// NewHandlers creates a new Handlers instance
func NewHandlers(db *DB, adminAPIKey string, heartbeatInterval int) *Handlers {
	return &Handlers{
		db:                db,
		adminAPIKey:       adminAPIKey,
		heartbeatInterval: heartbeatInterval,
	}
}

// HandleRegister handles POST /v1/agents/register
func (h *Handlers) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed")
		return
	}

	var req RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_json", "Failed to parse request body")
		return
	}

	// Validate request
	if req.APIKey == "" {
		h.writeError(w, http.StatusBadRequest, "missing_api_key", "api_key is required")
		return
	}
	if len(req.Models) == 0 {
		h.writeError(w, http.StatusBadRequest, "missing_models", "At least one model is required")
		return
	}

	// Generate agent ID
	agentID := uuid.New().String()

	// Hash the API key
	apiKeyHash, err := bcrypt.GenerateFromPassword([]byte(req.APIKey), bcrypt.DefaultCost)
	if err != nil {
		log.Printf("Error hashing API key: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "Failed to process registration")
		return
	}

	// Serialize capabilities
	capJSON, err := json.Marshal(req.Capabilities)
	if err != nil {
		log.Printf("Error marshaling capabilities: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "Failed to process registration")
		return
	}

	// Build agent and models
	agent := &Agent{
		ID:           agentID,
		APIKeyHash:   string(apiKeyHash),
		Name:         req.Name,
		Status:       "online",
		Capabilities: string(capJSON),
	}

	var models []AgentModel
	for _, m := range req.Models {
		models = append(models, AgentModel{
			AgentID:      agentID,
			ModelName:    m.Name,
			Quantization: m.Quantization,
			MaxContext:   m.MaxContext,
		})
	}

	// Register in database
	if err := h.db.RegisterAgent(agent, models); err != nil {
		log.Printf("Error registering agent: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "Failed to register agent")
		return
	}

	log.Printf("Agent registered: %s (%s)", agentID, req.Name)

	// Send response
	resp := RegisterResponse{
		AgentID:          agentID,
		HeartbeatInterval: h.heartbeatInterval,
	}
	h.writeJSON(w, http.StatusCreated, resp)
}

// HandleHeartbeat handles POST /v1/agents/{id}/heartbeat
func (h *Handlers) HandleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only POST is allowed")
		return
	}

	// Extract agent ID from path: /v1/agents/{id}/heartbeat
	path := strings.TrimPrefix(r.URL.Path, "/v1/agents/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[1] != "heartbeat" {
		h.writeError(w, http.StatusBadRequest, "invalid_path", "Invalid path format")
		return
	}
	agentID := parts[0]

	// Verify agent exists
	exists, err := h.db.AgentExists(agentID)
	if err != nil {
		log.Printf("Error checking agent: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "Failed to process heartbeat")
		return
	}
	if !exists {
		h.writeError(w, http.StatusNotFound, "agent_not_found", "Agent not found")
		return
	}

	// Parse request body
	var req HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.writeError(w, http.StatusBadRequest, "invalid_json", "Failed to parse request body")
		return
	}

	// Serialize capabilities
	capJSON, err := json.Marshal(req.Capabilities)
	if err != nil {
		capJSON = []byte("{}")
	}

	// Update heartbeat
	if err := h.db.UpdateHeartbeat(agentID, string(capJSON)); err != nil {
		log.Printf("Error updating heartbeat: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "Failed to update heartbeat")
		return
	}

	// Send response
	resp := HeartbeatResponse{
		Acknowledged: true,
		NextInterval: h.heartbeatInterval,
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// HandleAdminAgents handles GET /v1/admin/agents
func (h *Handlers) HandleAdminAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET is allowed")
		return
	}

	// Check admin API key
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		h.writeError(w, http.StatusUnauthorized, "unauthorized", "Missing or invalid Authorization header")
		return
	}
	providedKey := strings.TrimPrefix(authHeader, "Bearer ")
	if subtle.ConstantTimeCompare([]byte(providedKey), []byte(h.adminAPIKey)) != 1 {
		h.writeError(w, http.StatusUnauthorized, "unauthorized", "Invalid admin API key")
		return
	}

	// Get all agents
	agents, err := h.db.GetAllAgents()
	if err != nil {
		log.Printf("Error getting agents: %v", err)
		h.writeError(w, http.StatusInternalServerError, "internal_error", "Failed to get agents")
		return
	}

	// Build response
	var adminAgents []AdminAgentInfo
	onlineCount := 0
	for _, a := range agents {
		if a.Status == "online" {
			onlineCount++
		}

		// Parse capabilities
		var caps Capabilities
		json.Unmarshal([]byte(a.Capabilities), &caps)

		// Get models
		models, err := h.db.GetAgentModels(a.ID)
		if err != nil {
			log.Printf("Error getting models for agent %s: %v", a.ID, err)
			continue
		}

		var modelInfos []ModelInfo
		for _, m := range models {
			modelInfos = append(modelInfos, ModelInfo{
				Name:         m.ModelName,
				Quantization: m.Quantization,
				MaxContext:   m.MaxContext,
			})
		}

		adminAgents = append(adminAgents, AdminAgentInfo{
			AgentID:       a.ID,
			Name:          a.Name,
			Status:        a.Status,
			LastHeartbeat: a.LastHeartbeat,
			CurrentLoad:   a.CurrentLoad,
			Capabilities:  caps,
			Models:        modelInfos,
		})
	}

	resp := AdminResponse{
		Agents: adminAgents,
		Total:  len(adminAgents),
		Online: onlineCount,
	}
	h.writeJSON(w, http.StatusOK, resp)
}

// HandleHealth handles GET /health
func (h *Handlers) HandleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Only GET is allowed")
		return
	}

	// Check database
	if err := h.db.Ping(); err != nil {
		h.writeError(w, http.StatusServiceUnavailable, "unhealthy", "Database connection failed")
		return
	}

	h.writeJSON(w, http.StatusOK, map[string]string{
		"status": "healthy",
	})
}

// writeJSON writes a JSON response
func (h *Handlers) writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// writeError writes an error response
func (h *Handlers) writeError(w http.ResponseWriter, status int, code, message string) {
	h.writeJSON(w, status, ErrorResponse{
		Error:   code,
		Message: message,
	})
}
