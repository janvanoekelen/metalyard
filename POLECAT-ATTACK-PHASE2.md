# Polecat Attack: Phase 2 Design Review

**Attacker**: Onyx (metalyard/polecats/onyx)
**Date**: 2026-01-19
**Target Documents**:
- GPU_POOLING_ARCHITECTURE.md (Alan)
- gpu-detection-design.md (Albert)
- CENTRAL-SERVER-DESIGN.md (Alex)

---

## Executive Summary

The Phase 1 design has a **fatal architectural flaw**: it assumes direct client-to-agent connections while acknowledging that agents are behind NAT. The "data plane separation" described in Alex's document is impossible as designed.

Beyond this showstopper, the documents exhibit classic over-engineering: premature optimization formulas, multiple TBD choices that should be decided now, and scope creep disguised as "future optimization."

**Recommendation**: Strip this down to relay-only mode, pick concrete technologies, and ship something that works.

---

## 1. The NAT Problem (Showstopper)

### What the design says:
> "The client uses this ticket to connect directly to the GPU." (Alex, CENTRAL-SERVER-DESIGN.md:239)

### Why this is broken:

Consumer machines are behind NAT. The server cannot give a client a ticket to connect to `192.168.1.50:8080` because:
- That's a private IP address
- The client is in a different network
- There is no inbound route to that agent

The design acknowledges this in "Open Questions":
> "NAT traversal: How do we handle GPUs behind restrictive NATs? TURN server? Hole punching?"

This is not an "open question." This is **the entire problem**. Without solving NAT traversal, the direct data plane architecture doesn't exist.

### The fix:

**Server-relayed data plane only.** All inference traffic goes through the central server. This is what Alan actually recommends in his doc ("Recommendation: Ship with relay-only"), but Alex's design ignores this and builds elaborate ticket machinery for direct connections.

Kill the ticket system. Kill direct connections. Relay everything. You can add TURN/direct mode later when you have actual users complaining about latency.

---

## 2. Over-Engineering Hall of Fame

### 2.1 The Capability Score Formula

From Alan's doc (line 48-57):
```
capability_score = f(
    available_vram,
    memory_bandwidth,
    fp16_tflops,
    quantization_support,
    historical_tokens_per_sec
)
```

**Problem**: You don't have this data. You don't have historical tokens/sec until you run the system. Memory bandwidth isn't reliably detectable. FP16 TFLOPS vary by workload.

**Fix**: Just use VRAM. Route to any GPU that can fit the model. Optimize later with real data.

### 2.2 The Load Balancing Formula

From Alex's doc (line 246-258):
```
Score(gpu) = w1 * (1 - load%)
           + w2 * (1 / (active_requests + 1))
           + w3 * (1 / avg_latency_ms)
           + w4 * reliability_score
           + w5 * affinity_bonus
```

**Problem**: The weights (0.25, 0.25, 0.20, 0.20, 0.10) are made up. You have no data to suggest these are correct.

**Fix**: Random selection among capable GPUs. Seriously. Add sophistication when you have evidence it matters.

### 2.3 The Health State Machine

From Alex's doc (line 159-176):
```
REGISTERING → HEALTHY → SUSPECT → DEAD → DRAINING → OFFLINE
```

**Problem**: Five states plus transitions for what is fundamentally a binary question: "Can I route to this agent?"

**Fix**: Two states: ONLINE and OFFLINE. Agent sends heartbeat → ONLINE. Misses 2 heartbeats → OFFLINE. Done.

### 2.4 The Detection Pipeline

From Albert's doc (line 15-24):
```
Layer 1: Vendor APIs (nvml, rocm-smi, Metal)
Layer 2: OS APIs (DXGI, sysfs, IOKit)
Layer 3: Generic fallback (OpenCL, Vulkan enumeration)
Layer 4: CPU-only mode
```

**Problem**: Four layers of detection for what is usually: "Is there an NVIDIA GPU? If not, is there Apple Silicon? If not, CPU."

**Fix**: Two paths: NVIDIA (nvml) and Apple Silicon (Metal). Everything else is a non-goal for v1.

---

## 3. Bad Ideas to Kill

### 3.1 vLLM

From Alan's doc (line 329):
> "Agent inference: llama.cpp (universal GPU support) or vLLM (if NVIDIA-only acceptable)"

**Kill vLLM.**
- vLLM is designed for datacenter deployments with CUDA GPUs
- vLLM does not run on macOS
- vLLM does not run on consumer AMD cards
- vLLM requires Python, adding huge deployment complexity
- You said "heterogeneous consumer GPUs" - vLLM contradicts this

**Keep llama.cpp only.** One inference backend. One set of bugs to fix.

### 3.2 gRPC

From Alan's doc (line 332):
> "Agent-server protocol: gRPC or WebSocket"

**Kill gRPC.**
- gRPC requires protobuf schema management
- gRPC debugging is harder than HTTP
- gRPC adds build complexity (code generation)
- You get no benefit over HTTP for this use case

**Use HTTP + Server-Sent Events.** It's what you need for streaming tokens anyway.

### 3.3 AMD ROCm on Linux

From Albert's doc:
> "ROCm-SMI if available for detailed AMD info"

**Kill ROCm support for v1.**
- ROCm officially supports: RX 7900 XTX, RX 7900 XT, RX 7900 GRE, and a few others
- That's maybe 5% of AMD GPUs
- ROCm installation is a nightmare (kernel module conflicts, version mismatches)
- Your users will spend hours debugging ROCm and blame your software

If someone has an AMD GPU on Linux, they can run CPU inference. Revisit when ROCm stops being a disaster.

### 3.4 Intel Arc

From Albert's doc (line 85):
> "Intel Arc discrete GPUs: Use Level Zero API or Vulkan"

**Kill Intel Arc support.**
- Market share is negligible
- Driver support is immature
- You have no users asking for this

### 3.5 Inference Sandboxing

From Alan's doc (line 216-229):
```
┌───────────────────────────┐
│    Inference Sandbox      │
│  - No network access      │
│  - Memory capped          │
│  - GPU access only        │
│  - Read-only model files  │
└───────────────────────────┘
```

**This is fantasy.**
- llama.cpp doesn't run in a sandbox
- GPU sandboxing doesn't exist on consumer hardware
- "Memory capped" isn't a thing you can do to GPU memory
- This adds no security and lots of complexity

**Kill the sandbox abstraction.** The agent runs inference. That's it.

### 3.6 KV Cache Checkpointing

From Alan's doc (line 255):
> "Mitigation: Checkpoint KV cache periodically for long generations (expensive, optional)"

**Kill this idea now** before someone tries to implement it.
- KV cache formats differ between GPU types
- Checkpointing requires serialization, which is slow
- "Optional" means it will never be tested and break when used

If inference fails, retry from the start. Simple.

---

## 4. macOS + Metal Landmines

### 4.1 "recommendedMaxWorkingSetSize" is Unreliable

From Albert's doc (line 103):
```objc
device.recommendedMaxWorkingSetSize  // Usable for ML
```

**Problem**: This value changes dynamically based on system memory pressure. It might say 24GB is available, then drop to 8GB when the user opens Chrome. You cannot reliably use this for capacity planning.

**Fix**: Measure actual memory available at model load time. Report to server. Accept that it will change.

### 4.2 App Napping Will Kill Your Agent

macOS aggressively suspends "idle" background processes. Your agent will:
1. Register with server
2. Get no requests for 5 minutes
3. Get App Napped
4. Stop sending heartbeats
5. Get marked OFFLINE
6. Wake up confused

**Fix**: Use `ProcessInfo.processInfo.beginActivity(options: .userInitiated)` to prevent App Napping. Document this clearly.

### 4.3 Thermal Throttling is Aggressive

M-series Macs throttle aggressively, especially MacBooks. The APIs for detecting this are poor.

**What you can detect**:
- CPU thermal state via IOKit (but not GPU specifically)
- Performance drop (tokens/sec decreases)

**What you cannot reliably detect**:
- GPU-specific thermal state on Apple Silicon
- When throttling will start

**Fix**: Monitor tokens/sec in production. If it drops 30%+ without load change, report degraded. Don't pretend you can measure thermals accurately.

### 4.4 Unified Memory Accounting is Different

From Albert's doc (line 114):
> "available_for_gpu: Heuristic (total - OS overhead - app usage), typically 75% of free RAM"

**Problem**: This heuristic is wrong. Apple Silicon doesn't have "GPU memory" - it's all system RAM. When you allocate GPU buffers, you're competing with:
- The OS
- Other apps
- The Metal driver's own allocations
- Swap pressure

**Fix**: Don't report "GPU memory" for Apple Silicon. Report "system memory available" and flag it as unified. Let the scheduler understand the difference.

---

## 5. Claude API Compatibility Confusion

### The design is confused about which API it's implementing.

From Alan's doc (line 129-142):
```
POST /v1/chat/completions  ← This is OpenAI's endpoint
```

From Alex's doc (line 349):
```
POST /v1/messages          ← This is Anthropic's endpoint
```

The documents use both interchangeably. They're different APIs:

| Aspect | OpenAI | Anthropic |
|--------|--------|-----------|
| Endpoint | `/v1/chat/completions` | `/v1/messages` |
| Request body | `messages: [{role, content}]` | `messages: [{role, content}]` |
| Response format | `choices: [{message: {...}}]` | `content: [{type, text}]` |
| Streaming | Different event format | `message_start`, `content_block_delta` |

**Pick one.** I recommend Anthropic's `/v1/messages` format since:
1. The bead says "Claude-API compatibility"
2. The streaming format in Alex's doc (line 392-412) is already Anthropic-style

But then fix Alan's doc which shows OpenAI's endpoint.

---

## 6. Hidden Operational Costs

### 6.1 PostgreSQL

From Alan's doc (line 330):
> "Server: Go with PostgreSQL"

**Costs nobody mentioned**:
- Backup strategy (pg_dump? Streaming replication?)
- Schema migrations (how do you deploy changes?)
- Connection pooling (PgBouncer? Built-in?)
- Monitoring (pg_stat_statements? Third-party?)

**Simplification**: SQLite for v1. Single file. No server. Backup = copy file. Migrate by deploying new binary with embedded migrations.

PostgreSQL when you hit SQLite's limits (which will be thousands of agents, not dozens).

### 6.2 WebSocket State

From Alex's doc (line 80-84):
> "WebSocket after registration... Server needs to push commands"

**Costs nobody mentioned**:
- WebSocket connections consume memory (connection state, buffers)
- 1000 agents = 1000 persistent connections
- Load balancer must support sticky sessions or WebSocket passthrough
- Reconnection logic for network blips

**Simplification**: HTTP polling with long-poll. Agent polls every 5s. Server responds immediately if there's a command, else holds connection for 30s.

Less elegant, more debuggable, no sticky sessions needed.

### 6.3 Multi-Platform Binaries

From Alan's doc (line 328):
> "Agent runtime: Go or Rust (single binary, easy deploy)"

**Costs of "single binary"**:
- llama.cpp needs different builds for CUDA, Metal
- You're not shipping one binary, you're shipping:
  - Windows + CUDA
  - Windows + CPU
  - Linux + CUDA
  - Linux + CPU
  - macOS + Metal (ARM)
  - macOS + CPU (ARM)
- Each needs testing, CI/CD, version matrix

**Fix**: Accept this reality. Document the build matrix. Don't pretend "single binary" when it's 6+ binaries.

---

## 7. Revised Architecture

### 7.1 Core Simplifications

| Original | Revised |
|----------|---------|
| Direct + relay data plane | Relay only |
| 5-state health machine | 2 states: ONLINE/OFFLINE |
| Capability score formula | VRAM threshold only |
| Load balancing formula | Random among capable |
| gRPC + WebSocket options | HTTP + SSE only |
| Ticket system for routing | Server proxies all requests |
| llama.cpp + vLLM | llama.cpp only |
| NVIDIA + AMD + Intel + Apple | NVIDIA + Apple only (v1) |

### 7.2 Data Flow (Simplified)

```
┌─────────┐         ┌─────────┐         ┌─────────┐
│  Client │ ──────> │ Server  │ ──────> │  Agent  │
│         │ <────── │ (relay) │ <────── │ (llama) │
└─────────┘         └─────────┘         └─────────┘
    HTTP/SSE           HTTP/SSE           HTTP
```

- Client talks to server only (no NAT issues)
- Server relays prompts to agent, streams tokens back
- Agent runs llama.cpp server, exposes local HTTP endpoint
- Server handles all auth, routing, rate limiting

### 7.3 Agent Registration (Simplified)

```
POST /api/v1/register
{
  "agent_id": "uuid",
  "platform": "darwin-arm64",
  "gpu": {
    "vendor": "apple",
    "model": "M2 Pro",
    "memory_gb": 32,
    "unified": true
  },
  "models_loaded": ["deepseek-coder-6.7b-q4"]
}

Response:
{
  "session_token": "...",
  "heartbeat_interval_sec": 30
}
```

Then agent polls:
```
GET /api/v1/heartbeat?token=...

Response (no command):
{"status": "ok", "next_poll_sec": 30}

Response (with command):
{"status": "command", "command": "load_model", "args": {...}}
```

No WebSocket. HTTP polling. Simple.

---

## 8. Concrete Technology Choices

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Agent language | **Go** | Single binary (per platform), good concurrency, easy HTTP |
| Agent inference | **llama.cpp server** | Cross-platform, battle-tested, HTTP API |
| Server language | **Go** | Match agent, good HTTP/SSE support |
| Database | **SQLite** | Zero ops, embedded, sufficient for thousands of agents |
| API protocol | **HTTP + SSE** | Universal compatibility, easy debugging |
| Agent-server protocol | **HTTP polling** | No sticky sessions, survives network blips |
| Auth | **API keys** (agents) + **JWT** (clients) | Simple, stateless |

---

## 9. Explicit Non-Goals (v1)

**Do not implement these in v1:**

1. **Direct client-to-agent connections** - Relay everything
2. **AMD ROCm support** - Too fragile, tiny user base
3. **Intel Arc support** - Negligible market share
4. **Multi-GPU tensor parallelism** - Complex, niche
5. **KV cache migration/checkpointing** - Retry from start on failure
6. **Inference sandboxing** - No realistic implementation
7. **vLLM or other inference backends** - llama.cpp only
8. **gRPC** - HTTP only
9. **PostgreSQL** - SQLite until you need more
10. **Sophisticated load balancing** - Random among capable GPUs
11. **Request queuing with wait times** - 503 if busy, client retries
12. **Intel Macs** - Only Apple Silicon

**Why explicit non-goals matter**: These are things someone will suggest. Document now that they're out of scope so you don't relitigate every week.

---

## 10. Summary of Changes Required

### GPU_POOLING_ARCHITECTURE.md (Alan)
- Remove "Direct mode" data plane option
- Remove capability_score formula (just use VRAM)
- Remove vLLM option
- Remove gRPC option
- Remove inference sandboxing section
- Remove KV cache checkpointing mention
- Fix API endpoint (use /v1/messages, not /v1/chat/completions)

### gpu-detection-design.md (Albert)
- Remove Layer 2 and Layer 3 fallbacks (just vendor APIs)
- Remove ROCm/AMD Linux support for v1
- Remove Intel Arc
- Add macOS App Napping warning
- Fix unified memory accounting for Apple Silicon

### CENTRAL-SERVER-DESIGN.md (Alex)
- Remove ticket system entirely
- Remove direct data plane
- Simplify health states to ONLINE/OFFLINE
- Remove load balancing formula
- Replace WebSocket with HTTP polling
- Fix API endpoint confusion (/v1/messages)

---

*Polecat Onyx, 2026-01-19*
