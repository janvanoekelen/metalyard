# Distributed GPU Pooling System - Phase 1 Architecture

## Overview

A system to pool heterogeneous consumer/prosumer GPUs across a distributed network for inference workloads. Unlike datacenter GPU scheduling, this system assumes unreliable hardware, unreliable networks, heterogeneous capabilities, and distributed ownership.

---

## 1. Desktop Agent Architecture

### 1.1 GPU Detection Strategies

#### NVIDIA (Windows/Linux)
- **Primary**: NVML (NVIDIA Management Library) via `nvmlDeviceGetCount()`, `nvmlDeviceGetHandleByIndex()`
- **Fallback**: Parse `nvidia-smi` output
- **Detected properties**: Compute capability, VRAM total/available, current utilization, temperature, power draw, driver version

#### AMD (Windows/Linux)
- **Linux**: ROCm/HIP APIs (note: consumer card support is limited)
- **Windows**: WMI queries + DirectML capability detection
- **Challenge**: ROCm officially supports limited consumer cards. Must maintain compatibility matrix.

#### Apple Silicon (macOS)
- **Detection**: IOKit framework or `system_profiler SPDisplaysDataType`
- **Inference**: Metal Performance Shaders / MLX
- **Key difference**: Unified memory model - "VRAM" is shared with system RAM

#### CPU Fallback
- **Detection**: CPUID for AVX/AVX2/AVX-512 support
- **Properties**: Core count, cache hierarchy, NUMA topology
- **Use case**: Small models, low-priority overflow

### 1.2 GPU Disqualification Criteria

A GPU is marked **ineligible** if:

| Condition | Rationale |
|-----------|-----------|
| VRAM < model minimum + 512MB headroom | Can't fit model + KV cache |
| NVIDIA compute capability < 7.0 | Poor FP16 performance |
| Driver version on blacklist | Known crash/corruption bugs |
| 3+ thermal throttle events in 24h | Hardware unreliable |
| Inference error rate > 5% | Something wrong with setup |

### 1.3 Capability Evaluation

For each eligible GPU, compute:

```
capability_score = f(
    available_vram,
    memory_bandwidth,
    fp16_tflops,
    quantization_support,  # INT8, INT4, GPTQ, AWQ
    historical_tokens_per_sec
)
```

This score determines which models the GPU can serve and at what priority.

### 1.4 Local Inference Execution

The agent runs an embedded inference server (llama.cpp server, vLLM, or similar):

- **Model loading**: On-demand, with LRU eviction when VRAM pressure
- **OOM handling**: Graceful abort + report, not crash
- **Metrics export**: tokens/sec, memory usage, queue depth, temperature
- **Isolation**: Inference process sandboxed from agent control process

### 1.5 Heartbeat & Registration

```
Agent → Server (every 30s):
{
    agent_id: "uuid",
    status: "available" | "busy" | "degraded" | "draining",
    current_load: 0.0-1.0,
    gpu_state: {
        temperature_c: 65,
        vram_used_mb: 4096,
        vram_total_mb: 8192,
        utilization_pct: 45
    },
    network_latency_ms: 23,
    models_loaded: ["llama-7b-q4"]
}
```

**Connection model**: Agent initiates and maintains connection to server. Server never initiates connections to agents (NAT-friendly).

---

## 2. Central Server Architecture

### 2.1 Registry Service

Maintains authoritative state for all agents:

```
agents_table:
    agent_id        UUID PRIMARY KEY
    public_key      BYTEA           -- for auth
    capabilities    JSONB           -- GPU specs, supported models
    status          ENUM            -- online, degraded, offline
    last_heartbeat  TIMESTAMP
    reliability     FLOAT           -- 0.0-1.0, earned over time
    region          VARCHAR         -- geographic hint

agent_history:
    agent_id        UUID
    event_type      ENUM            -- registered, went_offline, completed_request, failed_request
    timestamp       TIMESTAMP
    metadata        JSONB
```

### 2.2 Scheduler/Router

**Request flow:**
1. Client submits request with model requirement
2. Scheduler queries registry for capable, available agents
3. Agents ranked by: capability fit → reliability score → current load → latency
4. Top agent assigned; fallback list maintained
5. If assigned agent fails, automatic failover to next in list

**Key insight**: This is **capability-based routing**, not load balancing. A request for Llama-70B can only go to agents with sufficient VRAM. Traditional load balancers don't understand this.

### 2.3 API Layer

Claude/OpenAI-compatible REST API:

```
POST /v1/chat/completions
Authorization: Bearer <client_api_key>

{
    "model": "llama-7b",
    "messages": [...],
    "stream": true
}
```

Response streams via SSE. Client is unaware of which agent serves the request.

---

## 3. Control Plane vs Data Plane

### Control Plane (Central Server)

All coordination flows through server:
- Agent registration/deregistration
- Heartbeats and health reporting
- Task assignment
- Model management commands (load, unload, update)
- Metrics aggregation
- Authentication decisions

### Data Plane (Relay by Default)

**Default mode: Server-relayed**
```
Client ←→ Server ←→ Agent
```
- Simplifies NAT/firewall handling
- Single point for logging, rate limiting, content filtering
- ~50-100ms added latency (acceptable for LLM inference)

**Optimized mode: Direct (future)**
```
Client ←→ Agent (with server-provided session token)
```
- Lower latency
- Requires: agent reachable (no NAT), client trusted
- Server still handles setup/teardown

**Recommendation**: Ship with relay-only. Direct mode is optimization for later.

---

## 4. Security Model

### 4.1 Authentication

| Party | Authenticates To | Mechanism |
|-------|------------------|-----------|
| Agent | Server | Long-lived API key + mTLS |
| Client | Server | API key (scoped permissions) |
| Server | Agent | (Implicit via agent-initiated connection) |

Agents and clients never directly authenticate to each other.

### 4.2 Trust Model

```
                    ┌──────────┐
                    │  Server  │
                    │ (Trusted)│
                    └────┬─────┘
                         │
        ┌────────────────┼────────────────┐
        │                │                │
        ▼                ▼                ▼
   ┌─────────┐     ┌─────────┐      ┌─────────┐
   │ Agent A │     │ Agent B │      │ Client  │
   │(Verified)│    │(Verified)│     │(AuthZ'd)│
   └─────────┘     └─────────┘      └─────────┘
```

- Server is the trust anchor
- Agents verified during onboarding (how? TBD - could be manual approval, hardware attestation, reputation stake)
- Clients authorized via API keys with permission scopes

### 4.3 Sandboxing

Agent architecture:

```
┌─────────────────────────────────────┐
│           Agent Process             │
│  (network access, control logic)    │
│                                     │
│    ┌───────────────────────────┐    │
│    │    Inference Sandbox      │    │
│    │  - No network access      │    │
│    │  - Memory capped          │    │
│    │  - GPU access only        │    │
│    │  - Read-only model files  │    │
│    └───────────────────────────┘    │
└─────────────────────────────────────┘
```

Defense in depth: even if a model or input triggers unexpected behavior, it can't phone home or access agent credentials.

### 4.4 Abuse Prevention

- **Rate limiting**: Per API key, per model, global
- **Cost accounting**: Track compute consumed, enforce quotas
- **Content filtering**: Optional, at server layer
- **Agent self-defense**: Agents can refuse requests (e.g., prompt too long, suspicious patterns)
- **Reputation system**: Misbehaving agents lose reliability score

---

## 5. Failure Modes & Handling

### 5.1 GPU Disappears Mid-Request

**Detection**: NVML/ROCm error during inference, process crash
**Handling**:
1. Agent reports failure to server with partial progress
2. Server marks request as failed-retryable
3. Server reassigns to next capable agent
4. Client sees increased latency, possibly repeated tokens (if not idempotent)

**Mitigation**: Checkpoint KV cache periodically for long generations (expensive, optional)

### 5.2 Laptop Sleeps

**Detection**: Heartbeat timeout (server-side), network disconnect (agent-side on wake)
**Handling**:
1. Server marks agent "degraded" after 1 missed heartbeat (30s)
2. Server marks agent "offline" after 3 missed (90s)
3. In-flight requests reassigned at "degraded" threshold
4. On wake, agent reconnects, re-registers, syncs state

### 5.3 Thermal Throttling

**Detection**: Agent monitors GPU temp, performance drop
**Handling**:
1. Agent reports "degraded" status with `reason: thermal`
2. Server reduces routing priority (not zero - still capable)
3. Agent can self-limit: refuse new requests until temp < threshold
4. If critical (>95C), agent should abort current work and report

### 5.4 Bad Drivers

**Detection**: CUDA/ROCm errors, inference corruption, crashes
**Handling**:
1. Agent catches errors, reports to server
2. Server marks agent "unhealthy"
3. Admin notification for manual investigation
4. Agent can attempt driver-version-specific workarounds

**Prevention**: Maintain driver blacklist, warn users during registration

### 5.5 Network Flakiness

**Detection**: Heartbeat jitter, increased latency, packet loss
**Handling**:
1. Server tracks heartbeat consistency
2. Jittery agents get lower reliability scores
3. Reconnection uses exponential backoff (1s, 2s, 4s, ... 60s max)
4. Long disconnect → full re-registration required

---

## 6. Why This Is NOT Kubernetes

| Kubernetes Assumes | Our Reality |
|--------------------|-------------|
| Nodes are servers in racks | Nodes are laptops, desktops, random machines |
| Network is datacenter fabric | Network is home internet, NAT, firewalls |
| Nodes are always on | Nodes sleep, reboot, get unplugged |
| Homogeneous workloads | Heterogeneous GPU hardware |
| Cluster admin manages | Distributed ownership, no central IT |
| Failures are exceptional | Failures are routine |

**What K8s would require:**
- Running kubelet on consumer machines (security nightmare, resource hog)
- Overlay networking through NAT (Flannel/Calico don't handle this well)
- Custom device plugins for NVIDIA, AMD, Apple (separate codebases)
- Fighting scheduling assumptions built for datacenters
- Explaining to users why they need to install a container runtime

**What we actually need:**
- Single lightweight binary that "just runs"
- Works behind NAT with zero configuration
- Handles heterogeneous hardware as first-class concept
- Degrades gracefully by design
- No container runtime, no cluster concepts

---

## 7. Key Components

| Component | Technology Recommendation |
|-----------|---------------------------|
| Agent runtime | Go or Rust (single binary, easy deploy) |
| Agent inference | llama.cpp (universal GPU support) or vLLM (if NVIDIA-only acceptable) |
| Server | Go with PostgreSQL |
| API layer | Standard HTTP/2, SSE for streaming |
| Agent-server protocol | gRPC or WebSocket (long-lived, bidirectional) |
| Metrics | Prometheus format, optional export |

---

## 8. Non-Obvious Design Decisions

### Decision 1: Relay-First Data Plane

**Choice**: All inference traffic goes through central server by default.

**Why non-obvious**: Direct connections are lower latency. Why add a hop?

**Rationale**:
- NAT traversal is hard. Really hard. STUN/TURN adds complexity and still fails in corporate environments.
- Relay gives us one place to log, audit, rate-limit, filter.
- For LLM inference, 50-100ms extra latency is negligible compared to generation time.
- Direct mode can be added later as optimization for known-good network paths.

### Decision 2: Agent-Initiated Connections Only

**Choice**: Central server never opens connections to agents.

**Why non-obvious**: Bidirectional seems more flexible. Server could push commands directly.

**Rationale**:
- Consumer machines are behind NAT. Server can't reach them.
- Even if reachable, opening inbound ports is a security concern.
- Agent-initiated + long-lived connection gives us bidirectional communication anyway.
- Simplifies firewall rules: agents only need outbound HTTPS.

### Decision 3: Earned Reliability, Not Assumed

**Choice**: New agents start with low routing priority. Trust is earned through successful completions.

**Why non-obvious**: Seems unfair to new agents. Slows onboarding.

**Rationale**:
- We cannot verify hardware quality at registration time.
- A GPU that benchmarks well might crash under sustained load.
- Bad agents should impact few requests, then get deprioritized.
- Creates incentive for stable operation.
- Reliability score can decay if agent goes flaky later.

### Decision 4: Capability-Based Routing, Not Load Balancing

**Choice**: Scheduler picks agents based on whether they can run the model, not just who's least busy.

**Why non-obvious**: Standard practice is round-robin or least-connections.

**Rationale**:
- A 4GB VRAM GPU cannot run a 13B model. Period.
- "Load balancing" to incapable hardware wastes everyone's time.
- We need model-aware scheduling that understands VRAM requirements, quantization support, etc.
- This is more like Kubernetes node affinity than NGINX load balancing.

### Decision 5: Pessimistic Availability by Design

**Choice**: Assume any agent can disappear at any moment. Always have a fallback.

**Why non-obvious**: Optimizing for the happy path is usually more efficient.

**Rationale**:
- Consumer hardware WILL fail. Laptops sleep. Power goes out. Dogs trip on cables.
- Building for graceful degradation from day one is cheaper than retrofitting.
- Every request should have a fallback plan before it starts.
- "Hoping it won't fail" is not a strategy.

---

## 9. Open Questions for Phase 2

1. **Agent onboarding**: How do we verify legitimate agents? Manual approval? Hardware attestation? Reputation stake?
2. **Model distribution**: How do agents get models? Pre-download? On-demand from server? P2P?
3. **Billing/incentives**: If agents are contributed, how do we reward operators?
4. **Multi-tenancy**: Can different users have dedicated agent pools?
5. **Batching**: Can we batch requests across agents for efficiency?

---

## 10. Appendix: Request Lifecycle

```
1. Client sends POST /v1/chat/completions
2. Server authenticates client, checks quota
3. Server determines model requirements (VRAM, etc.)
4. Scheduler queries registry for capable, available agents
5. Scheduler ranks agents, picks top candidate + fallback list
6. Server assigns request to agent via control channel
7. Agent loads model (if not already loaded)
8. Agent runs inference
9. Agent streams tokens to server
10. Server streams tokens to client
11. On completion: server records metrics, updates agent reliability
12. On failure: server reassigns to fallback agent, step 6+
```
