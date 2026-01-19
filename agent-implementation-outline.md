# Desktop Agent Implementation Outline

**Author**: Albert (metalyard/crew/Albert)
**Parent Issue**: me-a2k (Phase 3: Implementation outline)
**Date**: 2026-01-19

---

## Constraints (from Phase 2)

- Single binary: Go + embedded llama.cpp via cgo
- GPU detection: Use llama.cpp's built-in detection
- Communication: HTTP polling (no WebSocket)
- Supported: NVIDIA + Apple Silicon only
- Single GPU per agent
- Max 34B models

---

## 1. Agent Binary Structure

```
gpu-agent/
├── main.go              # Entry point, CLI flags
├── config/
│   └── config.go        # Configuration loading
├── gpu/
│   └── detect.go        # Wrapper around llama.cpp detection
├── inference/
│   ├── runner.go        # Model loading and inference
│   └── llamacpp.go      # cgo bindings to llama.cpp
├── agent/
│   ├── agent.go         # Main agent lifecycle
│   ├── registration.go  # Server registration
│   └── poller.go        # Work polling loop
├── protocol/
│   └── types.go         # JSON message types
└── vendor/
    └── llama.cpp/       # Embedded llama.cpp source
```

### Build Artifacts

```
gpu-agent-darwin-arm64   # macOS Apple Silicon
gpu-agent-linux-amd64    # Linux x86_64 (NVIDIA)
gpu-agent-windows-amd64  # Windows x86_64 (NVIDIA)
```

Single static binary per platform. No runtime dependencies except GPU drivers.

### Configuration

```go
type Config struct {
    ServerURL     string        // Central server endpoint
    AgentID       string        // Unique agent identifier (generated on first run)
    ModelsDir     string        // Where to store downloaded models
    PollInterval  time.Duration // How often to poll for work (default: 1s)
    HeartbeatSec  int           // Heartbeat frequency (default: 30s)
    MaxConcurrent int           // Concurrent requests (default: 1)
}
```

Loaded from: `~/.gpu-agent/config.json` or environment variables.

---

## 2. Registration Flow

### Sequence

```
Agent                                    Server
  |                                        |
  |-- POST /api/agents/register ---------->|
  |   {agent_id, hostname, gpu_info}       |
  |                                        |
  |<------------ 200 OK -------------------|
  |   {agent_id, token, poll_url}          |
  |                                        |
  |-- GET /api/agents/{id}/heartbeat ----->|  (every 30s)
  |   Authorization: Bearer {token}        |
  |                                        |
  |<------------ 200 OK -------------------|
  |   {status: "ok", pending_work: 0}      |
```

### Registration Request

```go
type RegisterRequest struct {
    AgentID   string   `json:"agent_id"`   // UUID, persisted locally
    Hostname  string   `json:"hostname"`
    Platform  string   `json:"platform"`   // "darwin-arm64", "linux-amd64"
    GPU       GPUInfo  `json:"gpu"`
    Models    []string `json:"models"`     // Locally available models
    Version   string   `json:"version"`    // Agent version
}

type GPUInfo struct {
    Name       string `json:"name"`        // "NVIDIA RTX 4090", "Apple M2 Pro"
    Vendor     string `json:"vendor"`      // "nvidia", "apple"
    VRAM_MB    int    `json:"vram_mb"`     // Total VRAM (or unified memory estimate)
    ComputeCap string `json:"compute_cap"` // "8.9" for NVIDIA, "metal3" for Apple
}
```

### Registration Response

```go
type RegisterResponse struct {
    AgentID  string `json:"agent_id"`
    Token    string `json:"token"`     // Bearer token for subsequent requests
    PollURL  string `json:"poll_url"`  // Endpoint to poll for work
    Accepted bool   `json:"accepted"`  // false if GPU doesn't meet requirements
    Reason   string `json:"reason"`    // If not accepted, why
}
```

### GPU Detection (via llama.cpp)

```go
// gpu/detect.go
// Uses llama.cpp's ggml_backend_cuda_get_device_count() and similar

/*
#cgo LDFLAGS: -lllama -lggml
#include "llama.h"
#include "ggml-cuda.h"  // or ggml-metal.h

int detect_nvidia_devices() {
    return ggml_backend_cuda_get_device_count();
}

void get_nvidia_device_info(int idx, char* name, size_t* vram) {
    ggml_backend_cuda_get_device_description(idx, name, 128);
    *vram = ggml_backend_cuda_get_device_memory(idx);
}
*/
import "C"

func DetectGPU() (*GPUInfo, error) {
    // Try NVIDIA first
    if count := C.detect_nvidia_devices(); count > 0 {
        // Get first device info
        return detectNVIDIA(0)
    }

    // Try Apple Metal
    if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
        return detectAppleSilicon()
    }

    return nil, errors.New("no supported GPU found")
}
```

---

## 3. Work Polling Loop

### Main Loop

```go
func (a *Agent) Run(ctx context.Context) error {
    // Initial registration
    if err := a.register(ctx); err != nil {
        return fmt.Errorf("registration failed: %w", err)
    }

    // Start heartbeat goroutine
    go a.heartbeatLoop(ctx)

    // Main work loop
    ticker := time.NewTicker(a.config.PollInterval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-ticker.C:
            if a.canAcceptWork() {
                a.pollForWork(ctx)
            }
        }
    }
}

func (a *Agent) canAcceptWork() bool {
    return a.activeRequests.Load() < int32(a.config.MaxConcurrent)
}
```

### Poll Request/Response

```go
// GET /api/agents/{id}/poll
// Authorization: Bearer {token}

type PollResponse struct {
    Work *WorkItem `json:"work"` // nil if no work available
}

type WorkItem struct {
    RequestID string `json:"request_id"`
    Model     string `json:"model"`      // "llama-3-8b-q4_k_m"
    Prompt    string `json:"prompt"`
    MaxTokens int    `json:"max_tokens"`
    Params    InferenceParams `json:"params"`
    ResultURL string `json:"result_url"` // Where to POST results
}

type InferenceParams struct {
    Temperature float32 `json:"temperature"`
    TopP        float32 `json:"top_p"`
    TopK        int     `json:"top_k"`
    Stop        []string `json:"stop"`
}
```

### Polling Implementation

```go
func (a *Agent) pollForWork(ctx context.Context) {
    resp, err := a.httpGet(ctx, a.pollURL)
    if err != nil {
        a.logError("poll failed", err)
        return
    }

    var poll PollResponse
    if err := json.Unmarshal(resp, &poll); err != nil {
        a.logError("poll parse failed", err)
        return
    }

    if poll.Work != nil {
        a.activeRequests.Add(1)
        go func() {
            defer a.activeRequests.Add(-1)
            a.executeWork(ctx, poll.Work)
        }()
    }
}
```

---

## 4. Inference Execution

### Execution Flow

```go
func (a *Agent) executeWork(ctx context.Context, work *WorkItem) {
    // 1. Ensure model is loaded
    model, err := a.ensureModelLoaded(work.Model)
    if err != nil {
        a.reportError(ctx, work, err)
        return
    }

    // 2. Run inference
    result, err := a.runInference(ctx, model, work)
    if err != nil {
        a.reportError(ctx, work, err)
        return
    }

    // 3. Report result
    a.reportResult(ctx, work, result)
}
```

### Model Loading

```go
func (a *Agent) ensureModelLoaded(modelName string) (*LoadedModel, error) {
    a.modelMu.Lock()
    defer a.modelMu.Unlock()

    // Check if already loaded
    if m, ok := a.loadedModels[modelName]; ok {
        return m, nil
    }

    // Check if model file exists locally
    modelPath := filepath.Join(a.config.ModelsDir, modelName+".gguf")
    if _, err := os.Stat(modelPath); os.IsNotExist(err) {
        // Download from server (out of scope for MVP)
        return nil, fmt.Errorf("model not found: %s", modelName)
    }

    // Unload current model if different (single model at a time)
    if a.currentModel != nil && a.currentModel.Name != modelName {
        a.currentModel.Unload()
    }

    // Load via llama.cpp
    model, err := a.loadModel(modelPath)
    if err != nil {
        return nil, err
    }

    a.currentModel = model
    a.loadedModels[modelName] = model
    return model, nil
}
```

### Inference via llama.cpp

```go
// inference/llamacpp.go

/*
#include "llama.h"

// Simplified - actual implementation more complex
char* run_completion(llama_context* ctx, const char* prompt,
                     int max_tokens, float temp) {
    // Tokenize prompt
    // Sample tokens until max_tokens or stop
    // Return generated text
}
*/
import "C"

func (m *LoadedModel) Complete(prompt string, params InferenceParams) (string, error) {
    cPrompt := C.CString(prompt)
    defer C.free(unsafe.Pointer(cPrompt))

    result := C.run_completion(
        m.ctx,
        cPrompt,
        C.int(params.MaxTokens),
        C.float(params.Temperature),
    )
    defer C.free(unsafe.Pointer(result))

    return C.GoString(result), nil
}
```

### Result Reporting

```go
type InferenceResult struct {
    RequestID    string `json:"request_id"`
    AgentID      string `json:"agent_id"`
    Text         string `json:"text"`
    TokensUsed   int    `json:"tokens_used"`
    DurationMs   int64  `json:"duration_ms"`
    FinishReason string `json:"finish_reason"` // "stop", "length", "error"
}

func (a *Agent) reportResult(ctx context.Context, work *WorkItem, result *InferenceResult) {
    body, _ := json.Marshal(result)

    req, _ := http.NewRequestWithContext(ctx, "POST", work.ResultURL, bytes.NewReader(body))
    req.Header.Set("Authorization", "Bearer "+a.token)
    req.Header.Set("Content-Type", "application/json")

    resp, err := a.httpClient.Do(req)
    if err != nil {
        a.logError("result report failed", err)
        // Will retry on next heartbeat
        return
    }
    defer resp.Body.Close()
}
```

---

## 5. Error Handling

### Error Categories

| Category | Example | Response |
|----------|---------|----------|
| Transient | Network timeout | Retry with backoff |
| Model | OOM loading model | Report, skip model |
| Inference | CUDA error mid-generation | Report partial, restart |
| Fatal | GPU disappeared | Graceful shutdown |

### Error Reporting

```go
type ErrorReport struct {
    RequestID string `json:"request_id,omitempty"`
    AgentID   string `json:"agent_id"`
    Category  string `json:"category"`  // "transient", "model", "inference", "fatal"
    Error     string `json:"error"`
    Timestamp int64  `json:"timestamp"`
    Context   map[string]string `json:"context,omitempty"`
}

func (a *Agent) reportError(ctx context.Context, work *WorkItem, err error) {
    report := ErrorReport{
        AgentID:   a.agentID,
        Error:     err.Error(),
        Timestamp: time.Now().Unix(),
        Category:  categorizeError(err),
    }

    if work != nil {
        report.RequestID = work.RequestID
    }

    // POST to /api/agents/{id}/errors
    a.httpPost(ctx, a.errorURL, report)
}
```

### Retry Logic

```go
func (a *Agent) withRetry(op func() error) error {
    backoff := 1 * time.Second
    maxBackoff := 30 * time.Second

    for attempt := 0; attempt < 5; attempt++ {
        err := op()
        if err == nil {
            return nil
        }

        if !isTransient(err) {
            return err // Don't retry non-transient errors
        }

        time.Sleep(backoff)
        backoff = min(backoff*2, maxBackoff)
    }

    return errors.New("max retries exceeded")
}

func isTransient(err error) bool {
    // Network errors, 5xx responses, timeouts
    var netErr net.Error
    if errors.As(err, &netErr) && netErr.Timeout() {
        return true
    }
    // ... other transient checks
    return false
}
```

### Graceful Shutdown

```go
func (a *Agent) Shutdown(ctx context.Context) error {
    // 1. Stop accepting new work
    a.accepting.Store(false)

    // 2. Wait for in-flight work (with timeout)
    done := make(chan struct{})
    go func() {
        for a.activeRequests.Load() > 0 {
            time.Sleep(100 * time.Millisecond)
        }
        close(done)
    }()

    select {
    case <-done:
        // Clean exit
    case <-ctx.Done():
        a.logWarn("shutdown timeout, %d requests abandoned", a.activeRequests.Load())
    }

    // 3. Deregister from server
    a.httpPost(ctx, a.deregisterURL, nil)

    // 4. Unload model, free GPU memory
    if a.currentModel != nil {
        a.currentModel.Unload()
    }

    return nil
}
```

### Health Self-Check

```go
func (a *Agent) healthCheck() HealthStatus {
    status := HealthStatus{Healthy: true}

    // Check GPU is still accessible
    if _, err := gpu.DetectGPU(); err != nil {
        status.Healthy = false
        status.Issues = append(status.Issues, "GPU not detected: "+err.Error())
    }

    // Check model is loaded and responsive
    if a.currentModel != nil {
        if err := a.currentModel.Ping(); err != nil {
            status.Healthy = false
            status.Issues = append(status.Issues, "Model unresponsive: "+err.Error())
        }
    }

    // Check disk space for models
    if free := diskFreeBytes(a.config.ModelsDir); free < 10*1024*1024*1024 {
        status.Warnings = append(status.Warnings, "Low disk space")
    }

    return status
}
```

Health is reported with each heartbeat.

---

## 6. Minimal Vertical Slice

For a 30-45 day build by a small team:

**Week 1-2: Agent skeleton**
- Go binary with CLI flags
- Config loading
- llama.cpp cgo bindings (use existing go-llama.cpp as reference)
- Basic GPU detection wrapper

**Week 2-3: Server communication**
- Registration flow
- Heartbeat loop
- Work polling

**Week 3-4: Inference**
- Model loading
- Single completion request
- Result reporting

**Week 4-5: Error handling & polish**
- Retry logic
- Graceful shutdown
- Health checks
- Logging

**Week 5-6: Testing & hardening**
- Integration tests with real GPUs
- Network failure scenarios
- OOM recovery

---

## Dependencies

- Go 1.21+
- llama.cpp (vendored, specific commit)
- CUDA Toolkit 12.x (for NVIDIA builds)
- Xcode Command Line Tools (for macOS builds)

No external Go dependencies except stdlib where possible. Minimize supply chain risk.
