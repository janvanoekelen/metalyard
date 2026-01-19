# Central Server & API Design

> Contributor: Alex (metalyard/crew/Alex)
> Supporting: me-xwd (Alan's lead)
> Focus: Registry, Scheduler, API, Control/Data Plane Separation

---

## Overview

The central server coordinates a pool of consumer-grade GPUs for serving Hugging Face models. It does NOT perform inference—it orchestrates. The architecture follows a **thin control plane, fat data plane** principle: coordination flows through central, inference flows edge-to-client.

---

## 1. Registry Service

### 1.1 GPU Registration Protocol

**Registration Flow:**
```
Desktop Agent                    Central Registry
     |                                 |
     |-- POST /api/v1/register ------->|
     |   {                             |
     |     agent_id: uuid,             |
     |     auth_token: jwt,            |
     |     gpus: [...],                |
     |     models_available: [...],    |
     |     endpoint: "ip:port"         |
     |   }                             |
     |<-- 200 {session_id, heartbeat_interval, config} --|
     |                                 |
     |-- WS /api/v1/session/{id} ----->| (upgrade to WebSocket)
     |<========= bidirectional =======>| (heartbeats + commands)
```

**Registration Payload:**
```json
{
  "agent_id": "uuid-v4",
  "agent_version": "1.2.0",
  "auth_token": "jwt",
  "platform": {
    "os": "linux",
    "os_version": "Ubuntu 22.04",
    "arch": "x86_64"
  },
  "gpus": [
    {
      "gpu_id": "gpu-0",
      "vendor": "nvidia",
      "model": "RTX 4090",
      "vram_mb": 24576,
      "compute_capability": "8.9",
      "driver_version": "535.104.05",
      "cuda_version": "12.2",
      "status": "available"
    }
  ],
  "models_available": [
    {
      "model_id": "deepseek-coder-6.7b-instruct",
      "quantization": "q4_k_m",
      "vram_required_mb": 5500,
      "loaded": true
    }
  ],
  "network": {
    "endpoint": "192.168.1.50:8080",
    "nat_type": "symmetric",
    "public_ip": "auto-detect"
  },
  "capacity": {
    "max_concurrent_requests": 2,
    "max_context_tokens": 16384
  }
}
```

**Why WebSocket after registration?**
- HTTP polling wastes bandwidth and increases latency for status changes
- Server needs to push commands (load model, evict, shutdown)
- Heartbeats are lightweight over persistent connection
- Graceful degradation: if WS fails, fall back to HTTP polling

### 1.2 Capability Tracking

**GPU Capability Model:**
```
┌─────────────────────────────────────────────────────────────┐
│                     GPU Capability Record                    │
├─────────────────────────────────────────────────────────────┤
│ Static (set at registration):                                │
│   - Hardware: vendor, model, VRAM, compute capability        │
│   - Software: driver version, runtime version                │
│   - Limits: max batch size, max context length               │
│                                                              │
│ Dynamic (updated via heartbeat):                             │
│   - Current load (0-100%)                                    │
│   - VRAM used/free                                           │
│   - Temperature                                              │
│   - Active requests count                                    │
│   - Models currently loaded                                  │
│   - Estimated queue depth                                    │
│                                                              │
│ Derived (computed by registry):                              │
│   - Reliability score (based on history)                     │
│   - Average latency per model                                │
│   - Throughput (tokens/sec) per model                        │
│   - Uptime percentage (24h rolling)                          │
└─────────────────────────────────────────────────────────────┘
```

**Model Compatibility Matrix:**
The registry maintains a compatibility matrix: which GPU configurations can run which models at what quality levels.

```json
{
  "model": "deepseek-coder-6.7b-instruct",
  "requirements": {
    "min_vram_mb": 4500,
    "quantizations": {
      "q4_k_m": {"vram_mb": 5500, "quality": "good"},
      "q5_k_m": {"vram_mb": 6500, "quality": "better"},
      "f16": {"vram_mb": 14000, "quality": "best"}
    },
    "compute_capability": {
      "nvidia": "7.0+",
      "amd": "gfx1030+",
      "apple": "m1+"
    }
  }
}
```

### 1.3 Health & Heartbeat Handling

**Heartbeat Protocol:**
```
Agent --> Registry (every 15s over WebSocket)
{
  "type": "heartbeat",
  "timestamp": "2026-01-19T21:45:00Z",
  "gpu_states": [
    {
      "gpu_id": "gpu-0",
      "load_percent": 45,
      "vram_used_mb": 8200,
      "temperature_c": 68,
      "active_requests": 1
    }
  ],
  "models_loaded": ["deepseek-coder-6.7b-instruct"],
  "queue_depth": 0
}
```

**Health State Machine:**
```
                    ┌─────────────────┐
                    │   REGISTERING   │
                    └────────┬────────┘
                             │ registration success
                             ▼
    ┌──────────┐       ┌─────────────┐       ┌───────────┐
    │  DRAINING │◄──────│   HEALTHY   │──────►│  SUSPECT  │
    └──────────┘ admin  └─────────────┘ miss  └───────────┘
         │       drain        ▲    │           1 heartbeat
         │                    │    │                │
         ▼                    │    │                │ miss 2 more
    ┌──────────┐              │    │                ▼
    │  OFFLINE  │◄────────────┘    │         ┌───────────┐
    └──────────┘  graceful         └────────►│   DEAD    │
                  disconnect                  └───────────┘
                                              3 missed = dead
```

**Heartbeat Failure Handling:**
- Miss 1 heartbeat (15s): Mark SUSPECT, stop routing new requests
- Miss 2 heartbeats (30s): Prepare failover for in-flight requests
- Miss 3 heartbeats (45s): Mark DEAD, trigger failover, reassign work
- Agent reconnects: Re-verify state, gradual ramp-up of traffic

---

## 2. Scheduler/Router

### 2.1 Request Routing

**Routing Decision Flow:**
```
Incoming Request
       │
       ▼
┌──────────────────┐
│ Parse & Validate │
│ - Model requested│
│ - Token estimate │
│ - Priority tier  │
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│  Filter Eligible │
│  - Model loaded? │
│  - Capacity?     │
│  - Health OK?    │
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│   Score & Rank   │
│  - Latency est   │
│  - Load balance  │
│  - Affinity      │
└────────┬─────────┘
         │
         ▼
┌──────────────────┐
│  Route & Track   │
│  - Issue ticket  │
│  - Start timeout │
└──────────────────┘
```

**Routing Ticket:**
When a request is routed, the scheduler issues a ticket:
```json
{
  "ticket_id": "req-abc123",
  "issued_at": "2026-01-19T21:45:00Z",
  "expires_at": "2026-01-19T21:45:05Z",
  "gpu_endpoint": "192.168.1.50:8080",
  "model": "deepseek-coder-6.7b-instruct",
  "signature": "hmac-sha256-signature"
}
```

The client uses this ticket to connect directly to the GPU. The GPU validates the signature before accepting work.

### 2.2 Load Balancing

**Strategy: Weighted Least-Connections with Latency Bias**

```
Score(gpu) = w1 * (1 - load%)
           + w2 * (1 / (active_requests + 1))
           + w3 * (1 / avg_latency_ms)
           + w4 * reliability_score
           + w5 * affinity_bonus

Where:
  w1 = 0.25  (prefer idle GPUs)
  w2 = 0.25  (prefer fewer connections)
  w3 = 0.20  (prefer faster GPUs)
  w4 = 0.20  (prefer reliable GPUs)
  w5 = 0.10  (prefer same GPU for session continuity)
```

**Affinity Bonus:**
If a user has used a GPU recently (within 5 minutes), prefer that GPU. This improves cache locality (model weights stay warm) and reduces latency.

**Overflow Handling:**
When all GPUs are at capacity:
1. Queue request with estimated wait time
2. If wait > 30s, offer degraded service (smaller model, CPU fallback)
3. If wait > 60s, return 503 with retry-after header

### 2.3 Failover Handling

**Scenario: GPU Dies Mid-Request**

```
┌─────────┐     ┌─────────┐     ┌─────────┐
│  Client │     │ Central │     │  GPU A  │
└────┬────┘     └────┬────┘     └────┬────┘
     │               │               │
     │──request────►│               │
     │               │──route──────►│
     │◄──ticket─────│               │
     │               │               │
     │═══════════ streaming ════════│
     │               │               │
     │               │   GPU A dies  X
     │               │◄──timeout────│
     │               │               │
     │               │         ┌─────────┐
     │               │         │  GPU B  │
     │               │         └────┬────┘
     │◄──failover───│──reroute────►│
     │   ticket B    │               │
     │               │               │
     │═══════════ resume streaming ═│
```

**Failover Protocol:**
1. Scheduler detects GPU death (heartbeat timeout or connection drop)
2. For each in-flight request to that GPU:
   - If streaming hadn't started: transparent reroute
   - If streaming in progress: send failover event to client
3. Client receives new ticket, reconnects to GPU B
4. GPU B starts fresh (no state transfer—stateless inference)

**Non-Recoverable Cases:**
- All GPUs running requested model are dead → 503
- Request was near completion → client may retry from scratch

---

## 3. API Layer

### 3.1 Claude Code Compatibility

The API is designed as a **superset of the Anthropic Messages API**. Any Claude Code client can connect by:
1. Changing base URL to our endpoint
2. Using pool-issued API key

**Compatibility Mapping:**
```
Anthropic API                    Our API
─────────────────────────────────────────────────
POST /v1/messages          →     POST /v1/messages (same)
model: "claude-3-opus"     →     model: "deepseek-coder-6.7b"
                                 (or "auto" for best available)
stream: true               →     stream: true (same)
max_tokens: 4096           →     max_tokens: 4096 (same)
```

**Superset Extensions:**
```json
{
  "model": "deepseek-coder-6.7b",
  "messages": [...],
  "x_pool_options": {
    "prefer_gpu": "gpu-abc123",
    "max_latency_ms": 500,
    "fallback_models": ["starcoder-7b", "codellama-7b"],
    "priority": "high"
  }
}
```

The `x_pool_options` field is optional. Clients unaware of it get default behavior.

### 3.2 Request/Response Format

**Request (Messages API compatible):**
```json
POST /v1/messages
{
  "model": "deepseek-coder-6.7b-instruct",
  "max_tokens": 1024,
  "messages": [
    {
      "role": "user",
      "content": "Write a Python function to merge two sorted lists."
    }
  ],
  "stream": true
}
```

**Response (non-streaming):**
```json
{
  "id": "msg_abc123",
  "type": "message",
  "role": "assistant",
  "content": [
    {
      "type": "text",
      "text": "Here's a Python function..."
    }
  ],
  "model": "deepseek-coder-6.7b-instruct",
  "stop_reason": "end_turn",
  "usage": {
    "input_tokens": 25,
    "output_tokens": 150
  },
  "x_pool_meta": {
    "gpu_id": "gpu-abc123",
    "inference_ms": 1250,
    "queue_ms": 50
  }
}
```

### 3.3 Streaming Support

**Server-Sent Events (SSE) Format:**
```
event: message_start
data: {"type":"message_start","message":{"id":"msg_abc","model":"deepseek-coder-6.7b"}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Here's"}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" a"}}

... (more deltas)

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_stop
data: {"type":"message_stop"}
```

**Failover During Streaming:**
```
event: x_pool_failover
data: {"type":"x_pool_failover","reason":"gpu_timeout","new_ticket":"..."}
```

Client reconnects with new ticket to continue. This is a pool extension; unaware clients see connection drop and retry.

---

## 4. Control Plane vs Data Plane Separation

### 4.1 What Goes Through Central

**Control Plane (through central server):**
- Authentication & authorization
- GPU registration and health
- Request routing decisions (ticket issuance)
- Metrics aggregation
- Model catalog management
- User quotas and rate limiting
- Billing events

**Data Plane (direct edge-to-client):**
- Actual inference (prompt in, tokens out)
- Streaming token delivery
- Model weight loading (from HF Hub, not central)

### 4.2 Traffic Flow Diagram

```
┌─────────────────────────────────────────────────────────────────┐
│                         CONTROL PLANE                           │
│                      (Central Server)                           │
│  ┌─────────┐  ┌─────────┐  ┌─────────┐  ┌─────────┐            │
│  │Registry │  │Scheduler│  │  Auth   │  │ Metrics │            │
│  └────┬────┘  └────┬────┘  └────┬────┘  └────┬────┘            │
└───────┼────────────┼────────────┼────────────┼──────────────────┘
        │            │            │            │
        │ register   │ route      │ verify     │ report
        │ heartbeat  │ ticket     │ token      │ usage
        │            │            │            │
┌───────┴────────────┴────────────┴────────────┴──────────────────┐
│                                                                  │
│    ┌──────────┐              ┌──────────┐              ┌──────┐ │
│    │  GPU A   │              │  GPU B   │              │Client│ │
│    │ (Agent)  │              │ (Agent)  │              │      │ │
│    └─────┬────┘              └────┬─────┘              └──┬───┘ │
│          │                        │                       │     │
│          │◄═══════════════════════╪═══════════════════════╝     │
│          │     DATA PLANE         │     (direct inference)      │
│          │   (tokens stream)      │                             │
└──────────┴────────────────────────┴─────────────────────────────┘
```

### 4.3 Why This Separation?

**Scalability:**
- Control plane is lightweight (routing decisions are small)
- Data plane is heavy (token streaming is bandwidth-intensive)
- Central server doesn't become bottleneck for inference traffic

**Latency:**
- Direct connection eliminates one network hop
- Streaming tokens go straight to client
- Only routing decision adds latency (~10-50ms)

**Reliability:**
- Central server failure doesn't kill active streams
- GPUs can continue serving existing connections
- Only new requests fail during central outage

**Cost:**
- Central server bandwidth costs stay low
- Heavy lifting is distributed to edge GPUs
- No egress charges for streaming through central

---

## 5. Key Design Decisions

### Decision 1: Tickets Over Proxying

**Choice:** Issue signed routing tickets instead of proxying all traffic through central.

**Rationale:**
- Proxying would make central a bottleneck (token streaming is ~1KB/s per request)
- 1000 concurrent requests × 1KB/s = 1MB/s through central = expensive & fragile
- Tickets are 500 bytes, one-time. Sustainable at any scale.
- Trade-off: Clients need to handle failover events, but Claude Code already handles reconnection.

### Decision 2: WebSocket for Heartbeat, HTTP for API

**Choice:** WebSocket for agent-to-registry communication, HTTP+SSE for client API.

**Rationale:**
- WebSocket gives us server push for commands (drain, evict model, shutdown)
- HTTP+SSE is more compatible with existing tooling (curl, proxies, load balancers)
- Agents are our software (we control both ends); clients might be anything
- SSE failover is cleaner than WebSocket reconnection for inference

### Decision 3: Stateless Inference, No Request Migration

**Choice:** On failover, client restarts request from scratch on new GPU.

**Rationale:**
- Migrating KV cache state between GPUs is complex (different memory layouts, quantizations)
- Most requests are short (<5s); restart cost is acceptable
- Simplicity wins: no distributed state, no coordination protocol
- Future optimization: checkpoint long-running requests (but not MVP)

### Decision 4: Model-Aware Routing, Not Generic Load Balancing

**Choice:** Scheduler knows which models are loaded on which GPUs.

**Rationale:**
- Model loading takes 30-60 seconds; can't load on demand
- Affinity matters: keeping model hot on a GPU improves throughput
- Enables smart preloading based on request patterns
- Alternative (treat GPUs as generic): would require load-on-demand, unacceptable latency

### Decision 5: Superset API, Not Fork

**Choice:** Make our API a strict superset of Anthropic Messages API.

**Rationale:**
- Zero friction adoption: change URL, swap model name, done
- Pool-specific features are opt-in extensions (`x_pool_options`)
- Clients that don't know about us work unchanged
- We benefit from Claude Code's existing retry/error handling
- Future: could proxy to actual Claude for hybrid pool (local GPU + cloud fallback)

---

## 6. Component Summary

| Component | Responsibility | Scaling Model |
|-----------|---------------|---------------|
| Registry | GPU registration, capability tracking, health | Single leader, read replicas |
| Scheduler | Request routing, ticket issuance, load balancing | Stateless, horizontal |
| Auth | Token validation, user quotas | Stateless, horizontal |
| Metrics | Usage aggregation, billing events | Async, eventually consistent |
| API Gateway | Rate limiting, request parsing | Stateless, horizontal |

---

## 7. Open Questions for Alan

1. **NAT traversal**: How do we handle GPUs behind restrictive NATs? TURN server? Hole punching? Or require agents to have public endpoints?

2. **Model distribution**: Should central server host model files, or always pull from HF Hub? Bandwidth costs vs. control trade-off.

3. **Multi-GPU inference**: Should we support tensor parallelism across multiple GPUs on same machine? Adds complexity but enables larger models.

4. **Quota model**: Per-user token limits? Per-request limits? Time-based quotas? Needs to align with abuse prevention.

5. **Graceful degradation**: When pool is overloaded, do we queue, reject, or offer degraded service (smaller model)? All three?

---

*Design by Alex, metalyard/crew/Alex, 2026-01-19*
