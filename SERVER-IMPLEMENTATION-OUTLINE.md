# Server Implementation Outline

> Contributor: Alex (metalyard/crew/Alex)
> Supporting: me-a2k (Alan's lead)
> Phase 3: Practical implementation for 30-45 day build

---

## Revised Constraints (from Phase 2)

- **Database**: SQLite (single file, no ops burden)
- **Transport**: HTTP + SSE only (no WebSocket, no gRPC)
- **API**: Simple completions (not Claude-compatible)
- **Routing**: Round-robin among capable healthy agents
- **Data plane**: Relay through server (no direct agent connections)
- **Hardware**: NVIDIA + Apple Silicon only
- **Models**: Max 34B until distribution solved

---

## 1. Server Binary Structure

### 1.1 Single Binary, Single Process

```
gpupool-server
├── main.go              # Entry point, config loading
├── db/
│   ├── sqlite.go        # Connection pool, migrations
│   └── queries.go       # Prepared statements
├── registry/
│   ├── registry.go      # Agent state management
│   └── health.go        # Health check, expiry
├── router/
│   └── router.go        # Round-robin selection
├── api/
│   ├── server.go        # HTTP server setup
│   ├── agents.go        # /agents/* handlers
│   └── completions.go   # /v1/completions handler
└── relay/
    └── sse.go           # SSE proxy logic
```

### 1.2 Startup Sequence

```
main():
    config = loadConfig(os.Args, env)

    db = sqlite.Open(config.DBPath)
    db.Exec(migrations)

    registry = NewRegistry(db)
    router = NewRouter(registry)
    relay = NewRelay()

    // Background: expire stale agents every 30s
    go registry.RunExpiryLoop(30s)

    http.Handle("/agents/poll", agentPollHandler(registry))
    http.Handle("/v1/completions", completionsHandler(router, relay))
    http.Handle("/health", healthHandler(db))

    http.ListenAndServe(config.Addr)
```

### 1.3 Configuration

```go
type Config struct {
    Addr            string        // ":8080"
    DBPath          string        // "./gpupool.db"
    AgentTimeout    time.Duration // 90s (3 missed polls)
    PollInterval    time.Duration // 30s (tell agents)
    RequestTimeout  time.Duration // 300s (max inference time)
    MaxRequestQueue int           // 100 (per agent)
}
```

Load from env vars, flags, or config file. Env vars win for 12-factor.

---

## 2. Registry Schema (SQLite)

### 2.1 Schema

```sql
-- Agents table: one row per registered agent
CREATE TABLE agents (
    agent_id        TEXT PRIMARY KEY,
    api_key_hash    TEXT NOT NULL,           -- bcrypt hash
    status          TEXT NOT NULL DEFAULT 'offline',  -- online, offline, draining
    last_poll       INTEGER NOT NULL,        -- unix timestamp
    capabilities    TEXT NOT NULL,           -- JSON blob
    current_load    INTEGER NOT NULL DEFAULT 0,  -- active request count
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE INDEX idx_agents_status ON agents(status);
CREATE INDEX idx_agents_last_poll ON agents(last_poll);

-- Models table: which models each agent can serve
CREATE TABLE agent_models (
    agent_id        TEXT NOT NULL,
    model_name      TEXT NOT NULL,
    quantization    TEXT,                    -- q4_k_m, q5_k_m, f16
    max_context     INTEGER NOT NULL,
    PRIMARY KEY (agent_id, model_name),
    FOREIGN KEY (agent_id) REFERENCES agents(agent_id) ON DELETE CASCADE
);

CREATE INDEX idx_agent_models_model ON agent_models(model_name);

-- Request log: audit trail, debugging
CREATE TABLE request_log (
    request_id      TEXT PRIMARY KEY,
    agent_id        TEXT,
    model_name      TEXT NOT NULL,
    status          TEXT NOT NULL,           -- pending, streaming, completed, failed
    input_tokens    INTEGER,
    output_tokens   INTEGER,
    started_at      INTEGER NOT NULL,
    completed_at    INTEGER,
    error_message   TEXT
);

CREATE INDEX idx_request_log_agent ON request_log(agent_id);
CREATE INDEX idx_request_log_started ON request_log(started_at);
```

### 2.2 Why SQLite

- Zero ops: single file, no daemon, no connection management
- WAL mode: concurrent reads during writes
- Transactions: agent state updates are atomic
- Good enough: 10K writes/sec is plenty for agent registry
- Backup: `cp gpupool.db gpupool.db.bak`

### 2.3 Prepared Statements

```go
var (
    stmtGetHealthyAgents = `
        SELECT agent_id, capabilities, current_load
        FROM agents
        WHERE status = 'online'
          AND last_poll > ?
          AND agent_id IN (
              SELECT agent_id FROM agent_models WHERE model_name = ?
          )
        ORDER BY current_load ASC`

    stmtUpdateAgentPoll = `
        UPDATE agents
        SET last_poll = ?, capabilities = ?, status = 'online', updated_at = ?
        WHERE agent_id = ?`

    stmtIncrementLoad = `
        UPDATE agents SET current_load = current_load + 1 WHERE agent_id = ?`

    stmtDecrementLoad = `
        UPDATE agents SET current_load = current_load - 1 WHERE agent_id = ?`
)
```

---

## 3. Agent Poll Endpoint

### 3.1 Protocol

Agents poll server every 30 seconds. No persistent connections.

```
POST /agents/poll
Authorization: Bearer <agent_api_key>
Content-Type: application/json

{
    "agent_id": "uuid",
    "capabilities": {
        "gpu_vendor": "nvidia",
        "gpu_model": "RTX 4090",
        "vram_mb": 24576,
        "models": [
            {"name": "deepseek-coder-6.7b", "quantization": "q4_k_m", "max_context": 16384}
        ]
    },
    "status": "online",
    "active_requests": 0,
    "queue_depth": 0
}
```

Response:

```json
{
    "poll_interval_sec": 30,
    "pending_requests": []
}
```

### 3.2 Handler Pseudocode

```go
func agentPollHandler(registry *Registry) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Auth
        apiKey := extractBearerToken(r)
        agentID := registry.ValidateAgentKey(apiKey)
        if agentID == "" {
            http.Error(w, "unauthorized", 401)
            return
        }

        // Parse body
        var poll AgentPoll
        json.NewDecoder(r.Body).Decode(&poll)

        if poll.AgentID != agentID {
            http.Error(w, "agent_id mismatch", 400)
            return
        }

        // Update registry
        registry.UpdateAgent(agentID, poll.Capabilities, poll.Status)

        // No pending requests to push (agents pull via separate endpoint)
        json.NewEncoder(w).Encode(PollResponse{
            PollIntervalSec: 30,
        })
    }
}
```

### 3.3 Agent Expiry

Background goroutine marks agents offline if no poll in 90s:

```go
func (r *Registry) RunExpiryLoop(interval time.Duration) {
    ticker := time.NewTicker(interval)
    for range ticker.C {
        cutoff := time.Now().Add(-90 * time.Second).Unix()
        r.db.Exec(`UPDATE agents SET status = 'offline' WHERE last_poll < ? AND status = 'online'`, cutoff)
    }
}
```

---

## 4. Client Completions Endpoint

### 4.1 Request Format

Simple completions API. NOT Claude/OpenAI compatible (different field names, simpler structure).

```
POST /v1/completions
Authorization: Bearer <client_api_key>
Content-Type: application/json

{
    "model": "deepseek-coder-6.7b",
    "prompt": "def fibonacci(n):",
    "max_tokens": 256,
    "temperature": 0.7,
    "stream": true
}
```

### 4.2 Response Format (streaming)

SSE stream:

```
event: token
data: {"text": " if"}

event: token
data: {"text": " n"}

event: token
data: {"text": " <="}

...

event: done
data: {"input_tokens": 5, "output_tokens": 42, "finish_reason": "stop"}
```

Error mid-stream:

```
event: error
data: {"code": "agent_timeout", "message": "Agent stopped responding"}
```

### 4.3 Handler Pseudocode

```go
func completionsHandler(router *Router, relay *Relay) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        // Auth
        apiKey := extractBearerToken(r)
        if !validateClientKey(apiKey) {
            http.Error(w, "unauthorized", 401)
            return
        }

        // Parse request
        var req CompletionRequest
        json.NewDecoder(r.Body).Decode(&req)

        // Validate
        if req.Model == "" || req.Prompt == "" {
            http.Error(w, "model and prompt required", 400)
            return
        }

        // Route: find capable healthy agent
        agent, err := router.SelectAgent(req.Model)
        if err != nil {
            http.Error(w, "no agents available for model", 503)
            return
        }

        // Set up SSE
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("Connection", "keep-alive")
        flusher := w.(http.Flusher)

        // Relay: proxy request to agent, stream response to client
        requestID := uuid.New().String()
        err = relay.Stream(r.Context(), agent, req, w, flusher, requestID)

        if err != nil {
            // Error already sent via SSE error event
            log.Printf("relay error: %v", err)
        }
    }
}
```

---

## 5. SSE Relay Implementation

### 5.1 Architecture

Server acts as relay between client and agent. No direct connections.

```
Client ──HTTP POST──▶ Server ──HTTP POST──▶ Agent
Client ◀──SSE stream── Server ◀──SSE stream── Agent
```

### 5.2 Relay Pseudocode

```go
type Relay struct {
    client *http.Client  // with timeouts
}

func (r *Relay) Stream(
    ctx context.Context,
    agent *Agent,
    req CompletionRequest,
    clientWriter http.ResponseWriter,
    flusher http.Flusher,
    requestID string,
) error {
    // Increment agent load
    registry.IncrementLoad(agent.ID)
    defer registry.DecrementLoad(agent.ID)

    // Build request to agent
    agentURL := fmt.Sprintf("http://%s/v1/completions", agent.Endpoint)
    body, _ := json.Marshal(req)
    agentReq, _ := http.NewRequestWithContext(ctx, "POST", agentURL, bytes.NewReader(body))
    agentReq.Header.Set("Content-Type", "application/json")
    agentReq.Header.Set("X-Request-ID", requestID)

    // Send to agent
    resp, err := r.client.Do(agentReq)
    if err != nil {
        writeSSEError(clientWriter, flusher, "agent_unreachable", err.Error())
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != 200 {
        writeSSEError(clientWriter, flusher, "agent_error", resp.Status)
        return fmt.Errorf("agent returned %d", resp.StatusCode)
    }

    // Stream relay: read from agent, write to client
    scanner := bufio.NewScanner(resp.Body)
    for scanner.Scan() {
        line := scanner.Text()

        // Pass through SSE lines unchanged
        fmt.Fprintf(clientWriter, "%s\n", line)
        flusher.Flush()

        // Check for client disconnect
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
    }

    if err := scanner.Err(); err != nil {
        writeSSEError(clientWriter, flusher, "stream_error", err.Error())
        return err
    }

    return nil
}

func writeSSEError(w http.ResponseWriter, f http.Flusher, code, message string) {
    fmt.Fprintf(w, "event: error\ndata: %s\n\n",
        mustJSON(map[string]string{"code": code, "message": message}))
    f.Flush()
}
```

### 5.3 Timeout Handling

```go
func NewRelay() *Relay {
    return &Relay{
        client: &http.Client{
            Timeout: 300 * time.Second,  // Max request duration
            Transport: &http.Transport{
                ResponseHeaderTimeout: 30 * time.Second,  // Agent must start responding
                IdleConnTimeout:       90 * time.Second,
            },
        },
    }
}
```

---

## 6. Router (Round-Robin)

### 6.1 Selection Algorithm

Simple round-robin among capable, healthy agents:

```go
type Router struct {
    registry *Registry
    mu       sync.Mutex
    index    map[string]int  // model -> last used index
}

func (r *Router) SelectAgent(model string) (*Agent, error) {
    agents := r.registry.GetHealthyAgentsForModel(model)
    if len(agents) == 0 {
        return nil, ErrNoAgents
    }

    r.mu.Lock()
    defer r.mu.Unlock()

    idx := r.index[model]
    agent := agents[idx % len(agents)]
    r.index[model] = idx + 1

    return agent, nil
}
```

### 6.2 Why Round-Robin

- Predictable: no magic scoring formulas to debug
- Fair: all agents get equal traffic
- Simple: 10 lines of code
- Good enough: with 5-10 agents, sophisticated balancing adds little value

Fancier algorithms (weighted, least-connections) can come later if needed.

---

## 7. Failure Handling

### 7.1 Agent Goes Offline Mid-Request

**Detection**: HTTP client timeout or connection reset during relay.

**Response**:
1. Send SSE error event to client: `{"code": "agent_failed", "message": "..."}`
2. Decrement agent load counter
3. Log failure in request_log
4. Do NOT automatically retry (client decides)

**Why no automatic retry**:
- We don't know how much was generated
- Client may have received partial output
- Stateless is simpler: client retries from scratch

### 7.2 Agent Never Responds

**Detection**: `ResponseHeaderTimeout` (30s) fires before agent sends headers.

**Response**: Same as above. SSE error, log, no retry.

### 7.3 All Agents Offline

**Detection**: `router.SelectAgent()` returns `ErrNoAgents`.

**Response**: HTTP 503 with JSON body:
```json
{
    "error": "no_agents_available",
    "message": "No agents available for model deepseek-coder-6.7b",
    "retry_after_sec": 30
}
```

### 7.4 Agent Reports Degraded

Agent can poll with `status: "draining"`. Router excludes draining agents from selection. Existing requests complete normally.

### 7.5 Database Failures

SQLite in WAL mode is remarkably robust. If write fails:
- Agent poll: return 500, agent will retry in 30s
- Completions: continue with cached agent list, degrade gracefully

---

## 8. Vertical Slice: What Ships First

### Week 1-2: Core Loop
- SQLite schema + basic CRUD
- Agent poll endpoint (no auth yet)
- Completions endpoint (hardcoded single agent)
- SSE relay working end-to-end

### Week 3-4: Multi-Agent
- Round-robin router
- Agent expiry
- Load tracking (increment/decrement)
- Basic error handling

### Week 5-6: Production Hardening
- API key auth (bcrypt)
- Request logging
- Health endpoint
- Graceful shutdown
- Basic monitoring (Prometheus metrics)

### Deferred
- Fancy routing (weighted, least-connections)
- Request queuing
- Rate limiting
- Multi-region

---

## 9. Open Questions

1. **Agent endpoint discovery**: How does server know agent's endpoint? Agent provides in poll? Or server assigns port range?

2. **TLS**: Do we require TLS for agent connections? Adds complexity but needed for untrusted networks.

3. **Request queuing**: If all agents busy, queue or reject? Current design rejects with 503. Queuing adds state.

4. **Metrics**: Prometheus? StatsD? Built-in? Defer until we know what to measure.

---

*Implementation outline by Alex, metalyard/crew/Alex, 2026-01-19*
