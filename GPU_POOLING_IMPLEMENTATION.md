# GPU Pooling System - Implementation Outline

## Revised Architecture (Post-Polecat Attack)

Based on Phase 2 review, the architecture simplifies to:

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Data plane | Relay-only | NAT traversal is solved; direct connections add complexity for marginal latency gain |
| Protocol | HTTP + SSE | Ubiquitous, debuggable, no special client libraries |
| Server language | Go | Fast compile, easy deploy, good concurrency |
| Agent inference | llama.cpp | Proven, supports NVIDIA + Apple Silicon, active development |
| Database | SQLite | No external dependencies, good enough for registry scale |
| GPU support | NVIDIA + Apple Silicon | AMD/ROCm too fragile on consumer hardware |
| API style | Simple completions | Claude-compatible is scope creep; start minimal |
| GPU per agent | One | Multi-GPU scheduling is hard; defer it |
| Model size | Max 34B | Larger models need distribution; out of scope |

**Explicit Non-Goals:**
- AMD GPU support
- Multi-GPU inference
- Model sharding/distribution
- Claude API compatibility
- Direct (non-relayed) connections
- Kubernetes integration

---

## 1. Desktop Agent Implementation

### 1.1 Structure

```
gpu-agent/
├── cmd/
│   └── agent/
│       └── main.go           # Entry point
├── internal/
│   ├── detect/
│   │   └── gpu.go            # GPU detection (wraps llama.cpp)
│   ├── runner/
│   │   └── runner.go         # Model loading and inference
│   ├── heartbeat/
│   │   └── heartbeat.go      # Registration and health reporting
│   └── config/
│       └── config.go         # Agent configuration
├── go.mod
└── go.sum
```

### 1.2 GPU Detection

**Strategy**: Don't reinvent. Use llama.cpp's detection.

llama.cpp already handles:
- NVIDIA via CUDA
- Apple Silicon via Metal
- CPU fallback

The agent shells out to `llama-server --list-gpus` or uses llama.cpp as a library (cgo bindings exist).

```go
// internal/detect/gpu.go

type GPUInfo struct {
    ID          string  // "cuda:0", "metal:0", "cpu"
    Type        string  // "nvidia", "apple", "cpu"
    Name        string  // "RTX 4090", "M2 Max"
    VRAM_MB     int     // Available VRAM (0 for unified memory)
    ComputeCap  string  // "8.9" for NVIDIA, "apple3" for Metal
}

func DetectGPUs() ([]GPUInfo, error) {
    // Option A: Parse llama-server --list-gpus output
    // Option B: Use llama.cpp cgo bindings directly
    // Option C: On NVIDIA, use go-nvml; on macOS, use Metal APIs

    // Recommendation: Start with Option A (simplest),
    // upgrade to B if performance matters
}
```

**What disqualifies a GPU:**

```go
func (g GPUInfo) CanServe(modelVRAM_MB int) bool {
    if g.Type == "cpu" {
        return true // CPU always works (slowly)
    }
    if g.VRAM_MB < modelVRAM_MB + 512 { // 512MB headroom
        return false
    }
    if g.Type == "nvidia" && g.ComputeCap < "7.0" {
        return false // Pre-Volta too slow
    }
    return true
}
```

### 1.3 Model Runner

The agent embeds or shells to `llama-server`.

**Embedded approach (recommended):**
```go
// internal/runner/runner.go

type Runner struct {
    serverProcess *exec.Cmd
    baseURL       string  // http://localhost:<port>
    modelPath     string
    gpuID         string
}

func (r *Runner) Start(modelPath string, gpuID string, port int) error {
    r.serverProcess = exec.Command("llama-server",
        "-m", modelPath,
        "--port", strconv.Itoa(port),
        "--gpu-layers", "99",  // Offload all to GPU
        "-ngl", "99",
    )
    // Set GPU device via env: CUDA_VISIBLE_DEVICES for NVIDIA
    return r.serverProcess.Start()
}

func (r *Runner) Complete(prompt string, maxTokens int) (<-chan string, error) {
    // POST to http://localhost:<port>/completion
    // Stream response tokens back via channel
}

func (r *Runner) Stop() error {
    return r.serverProcess.Process.Kill()
}
```

**Model loading strategy:**
- Agent starts with no model loaded
- Server assigns model on first request
- Agent downloads model if not cached (from central model registry)
- Single model loaded at a time (swap = stop old, start new)

### 1.4 Heartbeat & Registration

```go
// internal/heartbeat/heartbeat.go

type Heartbeat struct {
    AgentID     string    `json:"agent_id"`
    Status      string    `json:"status"`      // "available", "busy", "degraded"
    GPU         GPUInfo   `json:"gpu"`
    LoadedModel string    `json:"loaded_model"` // empty if none
    Temperature int       `json:"temperature_c"`
    Uptime      int       `json:"uptime_sec"`
}

type RegistrationResponse struct {
    AgentID     string   `json:"agent_id"`     // Server assigns on first reg
    ServerTime  int64    `json:"server_time"`
    Models      []string `json:"models"`       // Available model list
}

func (h *HeartbeatClient) Register(serverURL string, gpu GPUInfo) (*RegistrationResponse, error) {
    // POST /api/v1/agents/register
    // Body: { gpu: GPUInfo }
    // Returns: { agent_id, server_time, models }
}

func (h *HeartbeatClient) SendHeartbeat(hb Heartbeat) error {
    // POST /api/v1/agents/{agent_id}/heartbeat
    // Body: Heartbeat
    // Returns: 200 OK or commands (load_model, shutdown)
}
```

**Heartbeat interval**: 30 seconds
**Missed heartbeat threshold**: 3 (90 seconds → marked offline)

### 1.5 Agent Main Loop

```go
func main() {
    // 1. Detect GPU
    gpus, _ := detect.DetectGPUs()
    gpu := gpus[0] // Single GPU for now

    // 2. Register with server
    reg, _ := heartbeat.Register(serverURL, gpu)
    agentID := reg.AgentID

    // 3. Main loop
    ticker := time.NewTicker(30 * time.Second)
    for {
        select {
        case <-ticker.C:
            hb := buildHeartbeat(agentID, gpu, runner)
            resp, _ := heartbeat.SendHeartbeat(hb)
            handleCommands(resp) // May trigger model load/unload

        case req := <-incomingRequests:
            // Received via polling or server push
            handleInferenceRequest(req)
        }
    }
}
```

---

## 2. Central Server Components

### 2.1 Structure

```
gpu-server/
├── cmd/
│   └── server/
│       └── main.go           # Entry point
├── internal/
│   ├── registry/
│   │   └── registry.go       # Agent registry (SQLite)
│   ├── scheduler/
│   │   └── scheduler.go      # Request routing
│   ├── api/
│   │   ├── handlers.go       # HTTP handlers
│   │   └── middleware.go     # Auth, logging
│   └── relay/
│       └── relay.go          # SSE proxy to agents
├── schema.sql                # SQLite schema
├── go.mod
└── go.sum
```

### 2.2 Registry (SQLite)

```sql
-- schema.sql

CREATE TABLE agents (
    id TEXT PRIMARY KEY,
    api_key_hash TEXT NOT NULL,
    gpu_type TEXT NOT NULL,        -- 'nvidia', 'apple', 'cpu'
    gpu_name TEXT NOT NULL,
    vram_mb INTEGER NOT NULL,
    compute_cap TEXT,
    status TEXT DEFAULT 'offline', -- 'online', 'busy', 'degraded', 'offline'
    loaded_model TEXT,
    last_heartbeat INTEGER,        -- Unix timestamp
    reliability REAL DEFAULT 0.5,  -- 0.0-1.0
    created_at INTEGER,
    updated_at INTEGER
);

CREATE INDEX idx_agents_status ON agents(status);
CREATE INDEX idx_agents_gpu_type ON agents(gpu_type);

CREATE TABLE requests (
    id TEXT PRIMARY KEY,
    client_id TEXT NOT NULL,
    agent_id TEXT,
    model TEXT NOT NULL,
    status TEXT DEFAULT 'pending', -- 'pending', 'running', 'completed', 'failed'
    created_at INTEGER,
    completed_at INTEGER,
    tokens_generated INTEGER
);

CREATE TABLE models (
    name TEXT PRIMARY KEY,
    vram_required_mb INTEGER NOT NULL,
    min_compute_cap TEXT,
    download_url TEXT
);
```

### 2.3 Registry Operations

```go
// internal/registry/registry.go

type Registry struct {
    db *sql.DB
}

func (r *Registry) RegisterAgent(gpu GPUInfo, apiKeyHash string) (string, error) {
    agentID := uuid.New().String()
    _, err := r.db.Exec(`
        INSERT INTO agents (id, api_key_hash, gpu_type, gpu_name, vram_mb, compute_cap, status, created_at, updated_at)
        VALUES (?, ?, ?, ?, ?, ?, 'online', ?, ?)
    `, agentID, apiKeyHash, gpu.Type, gpu.Name, gpu.VRAM_MB, gpu.ComputeCap, time.Now().Unix(), time.Now().Unix())
    return agentID, err
}

func (r *Registry) UpdateHeartbeat(agentID string, hb Heartbeat) error {
    _, err := r.db.Exec(`
        UPDATE agents
        SET status = ?, loaded_model = ?, last_heartbeat = ?, updated_at = ?
        WHERE id = ?
    `, hb.Status, hb.LoadedModel, time.Now().Unix(), time.Now().Unix(), agentID)
    return err
}

func (r *Registry) GetCapableAgents(model string) ([]Agent, error) {
    // Query agents where:
    // - status = 'online'
    // - vram_mb >= model.vram_required_mb
    // - compute_cap >= model.min_compute_cap (if nvidia)
    // Order by: reliability DESC, last_heartbeat DESC
}

func (r *Registry) MarkStaleAgentsOffline() error {
    threshold := time.Now().Add(-90 * time.Second).Unix()
    _, err := r.db.Exec(`
        UPDATE agents SET status = 'offline' WHERE last_heartbeat < ? AND status != 'offline'
    `, threshold)
    return err
}
```

### 2.4 Scheduler

```go
// internal/scheduler/scheduler.go

type Scheduler struct {
    registry *Registry
}

func (s *Scheduler) AssignRequest(model string) (*Agent, error) {
    agents, err := s.registry.GetCapableAgents(model)
    if err != nil {
        return nil, err
    }
    if len(agents) == 0 {
        return nil, ErrNoCapableAgents
    }

    // Simple strategy: pick highest reliability among available
    // Future: consider current load, prefer agents with model already loaded
    for _, agent := range agents {
        if agent.Status == "online" {
            return &agent, nil
        }
    }
    return nil, ErrNoAvailableAgents
}
```

**Scheduling strategy for MVP**: Round-robin among capable, available agents. Reliability scoring is a refinement for later.

### 2.5 API Layer

```go
// internal/api/handlers.go

// Client-facing endpoints
func (h *Handlers) HandleCompletions(w http.ResponseWriter, r *http.Request) {
    // POST /v1/completions
    // Body: { model, prompt, max_tokens, stream }

    var req CompletionRequest
    json.NewDecoder(r.Body).Decode(&req)

    // 1. Find capable agent
    agent, err := h.scheduler.AssignRequest(req.Model)
    if err != nil {
        http.Error(w, "no capable agents", http.StatusServiceUnavailable)
        return
    }

    // 2. Mark agent busy
    h.registry.UpdateStatus(agent.ID, "busy")

    // 3. Relay request to agent, stream response back
    h.relay.StreamCompletion(w, agent, req)

    // 4. Mark agent available
    h.registry.UpdateStatus(agent.ID, "online")
}

// Agent-facing endpoints
func (h *Handlers) HandleAgentRegister(w http.ResponseWriter, r *http.Request) {
    // POST /api/v1/agents/register
}

func (h *Handlers) HandleAgentHeartbeat(w http.ResponseWriter, r *http.Request) {
    // POST /api/v1/agents/{id}/heartbeat
}

func (h *Handlers) HandleAgentPollWork(w http.ResponseWriter, r *http.Request) {
    // GET /api/v1/agents/{id}/work
    // Long-poll: block until work available or timeout
}
```

### 2.6 Relay (SSE Proxy)

```go
// internal/relay/relay.go

func (r *Relay) StreamCompletion(w http.ResponseWriter, agent *Agent, req CompletionRequest) error {
    // Set SSE headers
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")

    // Forward request to agent (agent is polling /work endpoint)
    // Agent processes and streams tokens back via its response
    // We relay each token to client as SSE event

    flusher := w.(http.Flusher)

    for token := range r.getAgentTokenStream(agent.ID, req) {
        fmt.Fprintf(w, "data: %s\n\n", token)
        flusher.Flush()
    }

    fmt.Fprintf(w, "data: [DONE]\n\n")
    flusher.Flush()
    return nil
}
```

---

## 3. Protocol Definitions

### 3.1 Agent → Server

#### Registration

```
POST /api/v1/agents/register
Authorization: Bearer <agent_api_key>
Content-Type: application/json

{
    "gpu": {
        "type": "nvidia",
        "name": "RTX 4090",
        "vram_mb": 24576,
        "compute_cap": "8.9"
    }
}

Response 200:
{
    "agent_id": "a1b2c3d4-...",
    "server_time": 1705123456,
    "models": ["llama-7b-q4", "llama-13b-q4", "mistral-7b-q4"]
}
```

#### Heartbeat

```
POST /api/v1/agents/{agent_id}/heartbeat
Authorization: Bearer <agent_api_key>
Content-Type: application/json

{
    "status": "online",
    "loaded_model": "llama-7b-q4",
    "temperature_c": 65,
    "uptime_sec": 3600
}

Response 200:
{
    "ack": true,
    "commands": []  // or ["load_model:mistral-7b-q4"], ["shutdown"]
}
```

#### Poll for Work

```
GET /api/v1/agents/{agent_id}/work?timeout=30
Authorization: Bearer <agent_api_key>

Response 200 (work available):
{
    "request_id": "r1e2q3-...",
    "model": "llama-7b-q4",
    "prompt": "Hello, how are you?",
    "max_tokens": 100
}

Response 204 (no work, timeout):
(empty body)
```

#### Submit Result

```
POST /api/v1/agents/{agent_id}/result
Authorization: Bearer <agent_api_key>
Content-Type: application/json

{
    "request_id": "r1e2q3-...",
    "tokens": ["I", "'m", " doing", " well", "!"],
    "finished": true,
    "error": null
}

Response 200:
{
    "ack": true
}
```

### 3.2 Client → Server

#### Completion (Non-Streaming)

```
POST /v1/completions
Authorization: Bearer <client_api_key>
Content-Type: application/json

{
    "model": "llama-7b-q4",
    "prompt": "Hello, how are you?",
    "max_tokens": 100
}

Response 200:
{
    "id": "cmpl-abc123",
    "model": "llama-7b-q4",
    "choices": [{
        "text": "I'm doing well, thank you for asking!",
        "finish_reason": "stop"
    }],
    "usage": {
        "prompt_tokens": 6,
        "completion_tokens": 9,
        "total_tokens": 15
    }
}
```

#### Completion (Streaming)

```
POST /v1/completions
Authorization: Bearer <client_api_key>
Content-Type: application/json

{
    "model": "llama-7b-q4",
    "prompt": "Hello, how are you?",
    "max_tokens": 100,
    "stream": true
}

Response 200 (SSE):
data: {"choices":[{"text":"I"}]}

data: {"choices":[{"text":"'m"}]}

data: {"choices":[{"text":" doing"}]}

data: {"choices":[{"text":" well"}]}

data: [DONE]
```

### 3.3 Protocol Justifications

| Choice | Why |
|--------|-----|
| JSON | Universal, debuggable, no schema compilation step |
| HTTP | Works everywhere, through any proxy/firewall |
| SSE for streaming | Simpler than WebSocket, works with HTTP/1.1, auto-reconnect |
| Long-polling for agent work | Simpler than WebSocket, survives connection drops gracefully |
| Bearer tokens | Standard, stateless, easy to implement |
| Explicit agent polling | Agent initiates all connections; server never needs to reach agent |

**Why not WebSocket:**
- Requires persistent connection management
- Proxy/firewall issues in corporate environments
- Reconnection logic is complex
- SSE + long-polling achieves same result with simpler code

**Why not gRPC:**
- Requires protobuf compilation
- Less debuggable (binary protocol)
- Browser clients need grpc-web
- HTTP + JSON is "good enough" for our throughput

---

## 4. Thin Vertical Slice (30-45 Days)

### Week 1-2: Agent Core

**Goal**: Agent that detects GPU, registers, sends heartbeats

Deliverables:
- [ ] GPU detection wrapper (shell to llama-server --list-gpus)
- [ ] Registration HTTP call
- [ ] Heartbeat loop (30s interval)
- [ ] Config file loading (server URL, API key)
- [ ] Basic logging

**Test**: Agent runs, registers with mock server, heartbeats appear in server logs

### Week 2-3: Server Core

**Goal**: Server that tracks agents and their status

Deliverables:
- [ ] SQLite schema and migrations
- [ ] Agent registration endpoint
- [ ] Heartbeat endpoint
- [ ] Stale agent cleanup (cron or goroutine)
- [ ] Basic admin endpoint (list agents)

**Test**: Multiple agents register, heartbeat, show up in admin list

### Week 3-4: Inference Path

**Goal**: End-to-end inference request

Deliverables:
- [ ] Agent: Model loading via llama-server
- [ ] Agent: Work polling endpoint
- [ ] Agent: Inference execution
- [ ] Server: Scheduler (pick any capable agent)
- [ ] Server: /v1/completions endpoint (non-streaming)
- [ ] Server: Request → Agent → Response relay

**Test**: curl POST to server, get completion from agent

### Week 4-5: Streaming & Polish

**Goal**: Production-ready streaming

Deliverables:
- [ ] Server: SSE streaming response
- [ ] Agent: Token-by-token streaming to server
- [ ] Server: Relay streaming to client
- [ ] Error handling: agent dies mid-request
- [ ] Client authentication (API keys)
- [ ] Basic rate limiting

**Test**: Streaming works in browser, handles agent failure gracefully

### Week 5-6: Hardening

**Goal**: Survive hostile conditions

Deliverables:
- [ ] Agent: Graceful shutdown on SIGTERM
- [ ] Agent: Reconnection with backoff
- [ ] Server: Request timeout handling
- [ ] Server: Agent reliability tracking (increment on success, decrement on failure)
- [ ] Monitoring: Prometheus metrics endpoint
- [ ] Deployment: Dockerfile for server, installer script for agent

**Test**: Kill agents randomly, system recovers. Metrics visible in Prometheus.

---

## 5. What's NOT in the Vertical Slice

Deferred to future phases:

| Feature | Why Deferred |
|---------|--------------|
| AMD GPU support | ROCm too fragile; NVIDIA + Apple covers 90% of use case |
| Multi-GPU per agent | Scheduling complexity; single GPU is simpler |
| Model distribution | P2P, sharding, etc. are research problems |
| Claude-compatible API | Scope creep; simple completions is enough to prove value |
| Direct connections | Relay works; direct is optimization |
| Billing/quotas | Business logic; get the tech working first |
| Web dashboard | Admin CLI is enough for MVP |

---

## 6. Risk Mitigation

| Risk | Mitigation |
|------|------------|
| llama.cpp API changes | Pin to specific release, wrap in abstraction layer |
| SQLite performance | Single server can handle ~10K agents; shard later if needed |
| Agent crashes during inference | Server detects via missed heartbeat or timeout, reassigns |
| Model download slow | Pre-seed common models; download in background |
| Network partition | Agent reconnects with backoff; server marks offline after 90s |
| GPU memory leak | Restart llama-server between requests (crude but effective) |

---

## 7. Success Criteria for MVP

1. **10+ agents** running on heterogeneous hardware (mix of NVIDIA + Apple)
2. **100 req/min** sustained throughput through single server
3. **< 5% request failure rate** under normal operation
4. **Auto-recovery** when agent disappears (< 2 min to reassign)
5. **Streaming works** end-to-end with < 200ms time-to-first-token overhead

If we hit these, the architecture is validated and we can iterate.
