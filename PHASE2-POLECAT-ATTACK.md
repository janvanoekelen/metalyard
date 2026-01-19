# Phase 2: Polecat Attack on GPU Pooling Design

**Attacker**: Obsidian (metalyard/polecats/obsidian)
**Date**: 2026-01-19
**Target**: Phase 1 design documents (Alan, Albert, Alex)

---

## Executive Summary

The Phase 1 design is competent distributed systems thinking applied to the wrong problem. It builds infrastructure for a future that won't arrive before simpler alternatives win. The core issue: **you're designing a datacenter scheduler for a consumer hardware problem**.

Kill the complexity. Ship something that works on 10 machines before designing for 10,000.

---

## 1. Over-Engineering

### 1.1 Direct Connections Are Premature

**Problem**: Alex's design introduces ticket-based direct client-to-GPU connections as core architecture, while Alan's document correctly says "Ship with relay-only."

These documents contradict each other. The ticket system (CENTRAL-SERVER-DESIGN.md:227-239) is 200+ lines of complexity for a feature that shouldn't exist in v1.

**Kill it**: Relay all traffic through central. Period. For LLM inference, the 50-100ms relay overhead is noise compared to generation time. You will never need direct connections for the stated use case.

### 1.2 GPU Detection Is Solved

**Problem**: Albert's 399-line GPU detection design reimplements what llama.cpp already does.

llama.cpp's `llama_get_device_info()` and build-time detection already handle:
- CUDA/ROCm/Metal backend selection
- VRAM detection
- Compute capability checking
- Device enumeration

**Kill it**: Use llama.cpp's detection. Query the inference server for its capabilities. Don't maintain a parallel detection system.

### 1.3 The Capability Score Formula Is Academic

**Problem**: GPU_POOLING_ARCHITECTURE.md:49-58 proposes:
```
capability_score = f(
    available_vram,
    memory_bandwidth,
    fp16_tflops,
    quantization_support,
    historical_tokens_per_sec
)
```

This is fake precision. You don't have accurate memory bandwidth numbers for consumer cards. FP16 TFLOPS vary wildly with thermal state. Historical tokens/sec depends on model, quantization, context length, and batch size.

**Kill it**: Use VRAM as the only hard constraint. Route by: can it fit the model? Is it online? Done.

### 1.4 WebSocket Heartbeats Add Fragility

**Problem**: Alex's design (CENTRAL-SERVER-DESIGN.md:84) uses WebSocket for heartbeats because "HTTP polling wastes bandwidth."

A heartbeat is ~200 bytes. At 15s intervals, that's 48KB/day. This is not a bandwidth problem. WebSocket connections die silently, require reconnection logic, and don't work through many corporate proxies.

**Kill it**: HTTP POST heartbeats. Stateless. Trivial to debug. Works everywhere.

### 1.5 Affinity Bonus Is Premature Optimization

**Problem**: The load balancer includes "affinity bonus" to route users to recently-used GPUs (CENTRAL-SERVER-DESIGN.md:260-261).

This optimizes for cache locality that doesn't exist. Model weights are loaded once. KV cache is per-request. There's no "warm" state to preserve between requests.

**Kill it**: Round-robin among capable, healthy GPUs. Add affinity later if measurements show it matters (they won't).

---

## 2. Bad Ideas

### 2.1 Direct Client-to-Agent Connections

**Problem**: The ticket system exposes consumer machines directly to the internet.

Even with signed tickets, you're asking users to:
- Open firewall ports
- Handle NAT traversal
- Accept connections from unknown clients
- Trust that ticket validation is bulletproof

One bug in ticket validation = arbitrary network access to someone's home computer.

**Kill it**: Never expose agents to clients. All traffic through central relay. The "latency savings" aren't worth the security surface.

### 2.2 AMD ROCm Consumer Support

**Problem**: gpu-detection-design.md claims ROCm support for RDNA2/RDNA3 consumer cards.

This is fiction. ROCm officially supports approximately zero consumer GPUs. The "community" support is:
- Requires kernel patching
- Breaks with every driver update
- Different gfx versions need different workarounds
- Most users can't get it working

**Kill it**: NVIDIA + Apple Silicon only for v1. AMD support is a footnote: "if you've already got ROCm working, it might work." Don't invest engineering time.

### 2.3 Multi-GPU Tensor Parallelism

**Problem**: Multiple documents mention multi-GPU support (gpu-detection-design.md:159-161).

Tensor parallelism across consumer GPUs is a research project, not a product feature:
- Requires matched GPUs (same model, same VRAM)
- PCIe bandwidth is the bottleneck, not compute
- NVLink doesn't exist on consumer cards
- Pipeline parallelism needs complex orchestration

**Kill it**: One GPU per agent. If someone has multiple GPUs, they run multiple agents. Simple.

### 2.4 Claude API Compatibility Is A Trap

**Problem**: CENTRAL-SERVER-DESIGN.md:314-344 promises "superset of Anthropic Messages API" compatibility.

This creates false expectations:
- Different tokenizers = different token counts = different billing
- Different context windows (Claude: 200K, most open: 4-32K)
- No tool use in most open models
- Different system prompt handling
- Different stop sequences

The `x_pool_options` extension already breaks the "superset" claim. Clients need pool-specific error handling for failover events.

**Kill it**: Design your own API. Make it simple. Don't inherit Anthropic's complexity for models that don't need it.

---

## 3. Hidden Operational Costs

### 3.1 Model Distribution

**Problem**: The design assumes models magically appear on agents. Real cost:

- Llama 70B Q4: ~40GB download
- User's home internet: 50-100 Mbps realistic
- Download time: 1-2 hours per model

If you host models: egress bandwidth is $0.09/GB (AWS). 100 agents downloading 70B = $360 per model update.

If users download from HuggingFace: they wait hours, blame you, give up.

**Reality check**: Start with small models (7B-13B). Have agents pre-download during onboarding. Don't promise 70B support until you've solved distribution.

### 3.2 Driver Update Hell

**Problem**: The design assumes drivers work. Reality:

- NVIDIA updates monthly, breaks CUDA compatibility randomly
- ROCm breaks with kernel updates
- macOS updates break Metal in subtle ways
- Users don't read changelogs

You will spend 30% of support time on "my agent stopped working after an update."

**Reality check**: Pin minimum driver versions. Test against specific versions. Provide "this worked yesterday" diagnostics.

### 3.3 Support Burden

**Problem**: Consumer hardware is flaky. Users will blame you for:

- Thermal throttling (their case has no airflow)
- OOM (they're running Chrome with 200 tabs)
- Network issues (their ISP is garbage)
- Power supplies (their 500W PSU can't handle GPU spikes)

You're not just building software, you're becoming IT support for strangers' gaming rigs.

**Reality check**: Aggressive auto-diagnostics. Clear "this is your hardware's problem" messaging. Disqualify flaky agents fast.

### 3.4 Model Licensing

**Problem**: The design ignores model licensing. Many models have restrictions:

- Llama: Meta's license prohibits some commercial uses
- Mistral: Various license versions with different terms
- DeepSeek: License requires attribution

If users serve commercial traffic through unlicensed models, you have liability.

**Reality check**: Maintain a model allowlist. Only support models with clear licensing. Make users acknowledge terms.

---

## 4. macOS + Metal Landmines

### 4.1 Unified Memory Accounting

**Problem**: gpu-detection-design.md:114-116 handwaves unified memory:
> `available_for_gpu`: Heuristic (total - OS overhead - app usage), typically 75% of free RAM

This is wrong. macOS will happily let you allocate "available" memory, then:
- Swap to disk (crushing performance)
- Kill your process under memory pressure
- Silently reduce GPU allocation

There's no reliable way to know how much memory is "really" available for GPU use.

**Reality check**: On Apple Silicon, use conservative fixed allocations based on machine tier (M1: 6GB, M1 Pro: 12GB, M1 Max: 24GB, etc.). Don't trust runtime memory queries.

### 4.2 Metal/MLX Quantization Differences

**Problem**: The quantization tables in gpu-detection-design.md assume CUDA-style quantization.

MLX (Apple's ML framework) supports different quantizations:
- Q4 and Q8 work differently than llama.cpp's GGUF
- Some quantization formats aren't supported at all
- Performance characteristics are different (Metal prefers different batch sizes)

**Reality check**: Maintain separate capability matrices for Metal vs CUDA backends. Don't assume feature parity.

### 4.3 macOS Power Management

**Problem**: Design assumes GPUs are always available. macOS disagrees:

- MacBooks throttle aggressively on battery
- "Low Power Mode" reduces GPU clock by 50%+
- Thermal throttling kicks in earlier than spec
- Closing the lid = sleep = agent offline

**Reality check**: Detect and report power state. Warn users about battery/thermal throttling. Don't promise reliability from laptops.

### 4.4 No Meaningful GPU Detection

**Problem**: Albert's detection code (gpu-detection-design.md:99-112) for Apple Silicon is overcomplicated.

On Apple Silicon, there's exactly one GPU. It's always available. You don't need to detect it—you need to detect the *machine tier* (M1/M2/M3, base/Pro/Max/Ultra) and look up its capabilities from a table.

**Reality check**: Use `sysctl hw.model` to get machine identifier. Map to known capabilities. Don't pretend Metal device enumeration tells you anything useful.

---

## 5. Claude API Compatibility Assumptions

### 5.1 Tokenization Mismatch

**Problem**: The design assumes token counts are comparable. They're not.

- Claude uses a proprietary tokenizer
- Llama uses SentencePiece
- Mistral uses a different SentencePiece model
- DeepSeek uses yet another

"1000 tokens" means different things for different models. Billing, context limits, and rate limits all become model-dependent.

**Reality check**: Don't expose "tokens" to users. Use characters or a synthetic "compute unit." Convert internally.

### 5.2 Context Window Mismatch

**Problem**: Claude has 200K context. Most open models:

- Llama 3: 8K (extended versions exist but quality degrades)
- Mistral: 32K
- DeepSeek Coder: 16K

Users will send Claude-sized contexts and get truncation or errors.

**Reality check**: Document context limits per model clearly. Fail fast with helpful errors. Don't silently truncate.

### 5.3 Tool Use Doesn't Exist

**Problem**: Claude's tool use is a major feature. Open models:

- Some have fine-tuned versions with function calling
- Quality is much lower than Claude
- Format varies by model

Pretending API compatibility means users expect tool use to work.

**Reality check**: Don't support tool use in v1. Document this clearly. It's not a capability of the pool.

### 5.4 System Prompt Handling

**Problem**: Claude has specific system prompt handling. Open models vary:

- Some expect system messages
- Some prepend system to first user message
- Some ignore system entirely
- Behavior changes with fine-tuning

**Reality check**: Document per-model system prompt behavior. Test and validate. Don't assume Claude semantics.

---

## 6. Revised Architecture

### 6.1 What To Build

```
┌─────────────────────────────────────────────────────────┐
│                   CENTRAL SERVER                         │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐     │
│  │   Registry  │  │   Router    │  │    Relay    │     │
│  │  (SQLite)   │  │  (in-proc)  │  │  (SSE proxy)│     │
│  └─────────────┘  └─────────────┘  └─────────────┘     │
└─────────────────────────────────────────────────────────┘
                          ▲
                          │ HTTPS only
                          │ (agents + clients)
                          ▼
┌─────────────────────────────────────────────────────────┐
│                      AGENTS                              │
│  ┌─────────────────────────────────────────────────┐   │
│  │  Single binary: agent + llama.cpp server        │   │
│  │  - Registers with central                        │   │
│  │  - Reports: VRAM, loaded models, health         │   │
│  │  - Receives work via HTTP long-poll             │   │
│  │  - Streams responses back through central       │   │
│  └─────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────┘
```

### 6.2 Technology Choices

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Agent | Go + embedded llama.cpp | Single binary, cross-platform, CGO for llama.cpp |
| Server | Go + SQLite | Simple, no external dependencies, good enough to 10K agents |
| Protocol | HTTPS + SSE | Works everywhere, debuggable, no WebSocket complexity |
| Inference | llama.cpp only | Widest hardware support, active development, quantization support |
| Models | GGUF format only | llama.cpp native, good quantization options |

### 6.3 What NOT To Build (Explicit Non-Goals)

1. **Direct client-to-agent connections** - All traffic through central relay
2. **AMD ROCm support** - NVIDIA + Apple only in v1
3. **Multi-GPU inference** - One GPU per agent
4. **Tool/function calling** - Not supported
5. **Custom GPU detection** - Use llama.cpp's detection
6. **Complex load balancing** - Round-robin among healthy, capable agents
7. **KV cache migration** - Restart on failover
8. **Model hosting** - Users download from HuggingFace
9. **70B+ models** - Max 34B until distribution is solved
10. **WebSocket anything** - HTTP only

### 6.4 Simplified API

```
POST /v1/completions
{
  "model": "llama-3-8b-q4",      // Exact model name, no "auto"
  "prompt": "...",               // Raw prompt, not messages
  "max_tokens": 512,
  "stream": true
}

Response (SSE):
data: {"token": "Hello"}
data: {"token": " world"}
data: {"done": true, "usage": {"prompt_tokens": 10, "completion_tokens": 2}}
```

No messages API. No Claude compatibility. Simple completions endpoint. Users who need chat formatting do it client-side.

### 6.5 Simplified Agent Registration

```
POST /v1/agents/register
{
  "agent_id": "uuid",
  "secret": "...",
  "vram_mb": 8192,
  "models": ["llama-3-8b-q4"],
  "platform": "darwin-arm64"
}

Response:
{
  "ok": true,
  "poll_url": "/v1/agents/{id}/poll",
  "poll_interval_sec": 10
}
```

Agent polls for work. No heartbeat protocol. If agent doesn't poll for 30s, it's offline. Simple.

---

## 7. Migration Path

If the simpler system works and you need to scale:

1. **Add PostgreSQL** when SQLite becomes bottleneck (>1000 agents)
2. **Add Redis** for session state if needed
3. **Add direct connections** if relay bandwidth becomes expensive (measure first)
4. **Add AMD support** if someone contributes it and maintains it
5. **Add tool calling** when open models support it reliably

But don't build any of this until you have the problem. The Phase 1 design is solving Year 3 problems in Month 1.

---

## 8. Summary: What To Do

1. **Delete** 60% of the Phase 1 design
2. **Build** a simple relay server with SQLite
3. **Ship** an agent that's just llama.cpp with a registration wrapper
4. **Support** NVIDIA + Apple Silicon only
5. **Limit** to 7B-34B models until distribution is solved
6. **Measure** everything before optimizing anything

The goal is "10 friends sharing GPUs" not "AWS competitor." Build for that.

---

*Attack by Obsidian, metalyard/polecats/obsidian, 2026-01-19*
