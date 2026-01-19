# Attack: Over-engineering and Operational Costs

**Reviewer**: metalyard/polecats/quartz
**Bead**: me-wii
**Documents Reviewed**: GPU_POOLING_ARCHITECTURE.md, gpu-detection-design.md, CENTRAL-SERVER-DESIGN.md
**Date**: 2026-01-19

---

## Summary

These designs have good foundations but suffer from classic Phase 1 disease: designing for scale and edge cases that don't exist yet. Several features should be cut entirely. Some will cause operational pain that isn't addressed. There are contradictions between documents.

**Verdict**: Cut 30-40% of the scope or this won't ship.

---

## 1. Over-engineering: Cut These

### 1.1 Four-Layer GPU Detection (gpu-detection-design.md)

**Problem**: The detection pipeline has 4 layers: Vendor APIs → OS APIs → Generic (OpenCL/Vulkan) → CPU.

**Why it's overkill**: Layer 3 (OpenCL/Vulkan enumeration) will almost never execute. If nvml/ROCm/Metal fail AND sysfs/DXGI/IOKit fail, something is seriously wrong with the machine. OpenCL won't save you.

**Cut**: Layer 3. Go straight from OS APIs to CPU fallback. Save 500-1000 lines of code and two dependency integrations.

### 1.2 Intel GPU Support (gpu-detection-design.md:84-86)

**The doc literally says**: "Usually integrated, rarely useful for LLM inference"

**Cut it**. You're adding code to support hardware you admit is useless for the task. Intel Arc discrete GPUs exist but are rare in the wild. Support them in Phase 3 when someone actually asks.

### 1.3 NUMA Topology Detection (GPU_POOLING_ARCHITECTURE.md:31)

**Problem**: CPU fallback includes NUMA topology detection.

**Reality**: Consumer machines don't have NUMA. Servers do. Your user base is "laptops, desktops, random machines" (your own words in Section 6). Multi-socket workstations are <1% of your target.

**Cut**: NUMA detection. Just count cores and check AVX support.

### 1.4 KV Cache Checkpointing (GPU_POOLING_ARCHITECTURE.md:255)

**Problem**: "Checkpoint KV cache periodically for long generations (expensive, optional)"

**Why it's a trap**: This is listed as "mitigation" for GPU-dies-mid-request. But:
- KV cache formats differ between backends (llama.cpp vs vLLM)
- Memory layouts differ between GPU vendors
- Quantization affects cache size
- You'd need to transfer GB of state over the network

**Cut**: Delete this line. If the GPU dies, restart the request. Checkpointing across heterogeneous GPUs is a research project, not a Phase 1 feature.

### 1.5 Direct Mode Data Plane (Contradictory Design)

**Problem**: GPU_POOLING_ARCHITECTURE.md says "Ship with relay-only. Direct mode is optimization for later." (line 176)

But CENTRAL-SERVER-DESIGN.md describes direct connections in detail: routing tickets, HMAC signatures, client connects directly to GPU endpoint.

**These docs contradict each other.**

**Decision required**: Either:
- Cut direct mode entirely from design (delete Section 4 of CENTRAL-SERVER-DESIGN.md except the principles)
- Or commit to direct mode and delete the relay-only recommendation

You can't have both. Having contradictory designs will confuse implementers and lead to half-built features.

### 1.6 Five-Weight Scoring Formula (CENTRAL-SERVER-DESIGN.md:246-258)

**Problem**:
```
Score(gpu) = w1 * (1 - load%)
           + w2 * (1 / (active_requests + 1))
           + w3 * (1 / avg_latency_ms)
           + w4 * reliability_score
           + w5 * affinity_bonus
```

**Why it's overkill**: Five weights with unclear interactions. What if a GPU is 80% loaded but has great latency and high reliability? The formula doesn't tell you which factor wins.

**Simpler alternative**:
1. Filter: can run model? healthy?
2. Sort by: reliability (high to low)
3. Tiebreak by: current load (low to high)

That's it. Three lines of logic, not five floating-point multiplications per GPU per request.

### 1.7 Session Affinity Bonus (CENTRAL-SERVER-DESIGN.md:260-261)

**Problem**: "If a user has used a GPU recently (within 5 minutes), prefer that GPU. This improves cache locality."

**What cache?** Model weights are always loaded. KV cache is per-request and discarded after. The only thing that stays "warm" is the model in VRAM, which is already there.

This adds a 5-minute tracking window per user×GPU combination for negligible benefit. Cut it.

### 1.8 WebSocket + HTTP Polling Fallback (CENTRAL-SERVER-DESIGN.md:84)

**Problem**: "Graceful degradation: if WS fails, fall back to HTTP polling"

**Reality**: Now you have two code paths to test. Two protocols to debug. Two sets of timing behaviors.

**Cut**: WebSocket only. If an agent can't maintain a WebSocket, it's offline. Modern networks support WebSocket. Don't add a fallback for 2010-era infrastructure.

### 1.9 Fallback Models in Requests (CENTRAL-SERVER-DESIGN.md:337)

**Problem**: API allows `"fallback_models": ["starcoder-7b", "codellama-7b"]`

**Complexity explosion**:
- Does the scheduler try them in order?
- What if model A is available but slow, and model B is faster but different?
- What if none are available?
- Does the client know which model actually ran?

This is a feature that sounds nice but adds significant routing complexity. Users will just pick one model.

**Cut**: Single model per request. If unavailable, return error. Let client retry with different model if they want.

---

## 2. Hidden Operational Costs

### 2.1 Model Distribution Is Undefined

**The elephant in the room**: All three docs punt on this.

- GPU_POOLING_ARCHITECTURE.md: "How do agents get models? Pre-download? On-demand from server? P2P?" (listed as Phase 2 question)
- CENTRAL-SERVER-DESIGN.md: "Model weight loading (from HF Hub, not central)"

**The problem you're not seeing**: If 1000 agents all try to download a 14GB model from Hugging Face simultaneously, that's 14TB of bandwidth. HF will rate-limit you. Your agents will timeout. Users will see "model loading" for 20 minutes.

**This needs a design before Phase 1**, not after. Options:
1. Pre-bundle models in agent installer (huge download)
2. Central server caches models and distributes (you pay bandwidth)
3. P2P model distribution (complex, adds attack surface)
4. Require users to pre-download (bad UX)

You can't ship without solving this.

### 2.2 Reliability Score Algorithm Missing

**Problem**: Multiple docs reference "reliability score" that's "earned over time" and "can decay." But there's no algorithm.

- How fast does a new agent ramp up?
- What's the decay rate for failures?
- How many successes to recover from one failure?
- Is it per-model or global?

**Operational pain**: You'll spend months tuning this in production, reacting to complaints of "my GPU never gets requests" or "bad GPU keeps getting traffic."

**Design this now**. Simple formula:
```
reliability = successes / (successes + failures + 1)
decay: multiply by 0.99 every hour
min_threshold: 0.7 to receive traffic
```

Or something. But write it down.

### 2.3 Heartbeat Math Doesn't Scale

**Inconsistency**: GPU_POOLING_ARCHITECTURE.md says 30s heartbeat. CENTRAL-SERVER-DESIGN.md says 15s.

**At 15s with 1000 agents**: 67 heartbeats/second hitting your registry.
**At 30s with 10000 agents**: 333 heartbeats/second.

Each heartbeat is a database write (updating last_heartbeat, GPU state). PostgreSQL single-leader will struggle.

**Mitigation needed**:
- Use Redis for heartbeat state (write-optimized)
- PostgreSQL for registration (read-heavy, stable)
- Or: longer heartbeat interval (60s?) with health degradation detection via inference failures

### 2.4 PostgreSQL for Real-Time Routing

**Problem**: "Single leader, read replicas" for registry (CENTRAL-SERVER-DESIGN.md:552)

**Reality**: The scheduler queries registry for EVERY routing decision. With streaming requests lasting seconds, you might have 100+ routing decisions per second at modest scale.

Read replicas have replication lag. If an agent goes offline and replica doesn't know yet, you route to a dead agent.

**Either**:
- Accept stale reads (route failures will happen)
- Use in-memory state with PostgreSQL as backup
- Design explicit consistency boundaries

### 2.5 Metrics "Optional" Is Wrong

**Problem**: GPU_POOLING_ARCHITECTURE.md lists Prometheus metrics as "optional export"

**Reality**: You're building a distributed system with unreliable nodes. You NEED metrics for:
- Detecting which agents are flaky
- Capacity planning
- Debugging routing issues
- Billing accuracy

**Not optional**. And Prometheus pull model doesn't work with agents behind NAT. You need push-based metrics (Prometheus remote write, or different system).

### 2.6 Content Filtering Is Expensive

**Problem**: "Content filtering: Optional, at server layer" (GPU_POOLING_ARCHITECTURE.md:238)

**What this actually requires**:
- Running ANOTHER model to classify output
- Or: maintaining blocklists and regex (whack-a-mole forever)
- Latency: adds 50-200ms per response
- Compute: non-trivial at scale

This is listed as a bullet point but is actually a product in itself. Either commit to it (design, budget, implement) or explicitly cut it.

---

## 3. Unnecessary Complexity

### 3.1 Three Health States

**Problem**: HEALTHY → SUSPECT → DEAD

**SUSPECT adds logic**:
- "Stop routing new requests" (but keep existing?)
- "Prepare failover" (what does this mean?)
- Then DEAD triggers actual failover

**Simpler**: HEALTHY or DEAD. Binary. If you miss N heartbeats, you're dead. When heartbeat resumes, you're healthy. Less state machine, less bugs.

### 3.2 Priority Tiers

**Problem**: API mentions `"priority": "high"` with no design.

Questions without answers:
- Does high priority queue-jump?
- Is there reserved capacity for high priority?
- Who gets to set priority?
- What happens if everyone sets high?

**Cut or design**. Don't leave half-specified features in the API.

### 3.3 Quantization Matrix Complexity

**Problem**: Capability tracking maintains per-model, per-GPU, per-quantization compatibility.

```json
"quantizations": {
  "q4_k_m": {"vram_mb": 5500, "quality": "good"},
  "q5_k_m": {"vram_mb": 6500, "quality": "better"},
  "f16": {"vram_mb": 14000, "quality": "best"}
}
```

**Reality**: You'll pick one quantization per model and deploy that. The combinatorics of "this GPU runs 7B-q4 but not 7B-q8, and 13B-q4 but only with reduced context" is scheduling hell.

**Simplify**: One quantization per model. If the model doesn't fit, it doesn't fit.

### 3.4 Claude AND OpenAI Compatibility

**Problem**: Doc says "Claude/OpenAI-compatible REST API" and "superset of Anthropic Messages API"

**But**: The SSE format matches Anthropic (message_start, content_block_delta, etc.), not OpenAI (which uses different event names).

Pick one. Implementing both SSE formats is double the work. You're targeting Claude Code, so match Anthropic's format. Drop the OpenAI reference.

---

## 4. Recommendations

### Must Cut (Blocking Phase 1)
1. Intel GPU support
2. OpenCL/Vulkan detection layer
3. KV cache checkpointing
4. Fallback models in requests
5. HTTP polling fallback (WebSocket only)
6. Session affinity bonus
7. NUMA topology

### Must Resolve (Contradictions)
1. Direct mode vs relay-only: pick one, delete the other from docs
2. Heartbeat interval: 15s or 30s?

### Must Design Before Phase 1
1. Model distribution strategy
2. Reliability score algorithm
3. Heartbeat storage (not PostgreSQL)

### Should Simplify
1. Health states: HEALTHY/DEAD (drop SUSPECT)
2. Routing score: filter → reliability → load (drop fancy formula)
3. Quantization: one per model (drop matrix)

---

## Appendix: Line-by-Line Cuts

| File | Lines | What to Cut |
|------|-------|-------------|
| gpu-detection-design.md | 22-23 | Layer 3 generic fallback |
| gpu-detection-design.md | 83-86 | Intel GPU section |
| GPU_POOLING_ARCHITECTURE.md | 31 | NUMA topology |
| GPU_POOLING_ARCHITECTURE.md | 255 | KV cache checkpoint |
| GPU_POOLING_ARCHITECTURE.md | 169-175 | Direct mode (if relay-only) |
| CENTRAL-SERVER-DESIGN.md | 84 | HTTP polling fallback |
| CENTRAL-SERVER-DESIGN.md | 246-258 | Five-weight formula |
| CENTRAL-SERVER-DESIGN.md | 260-261 | Session affinity |
| CENTRAL-SERVER-DESIGN.md | 337 | Fallback models |
| CENTRAL-SERVER-DESIGN.md | 225-240 | Routing tickets (if relay-only) |

---

*Attack complete. Kill the bad ideas. Ship something that works.*
