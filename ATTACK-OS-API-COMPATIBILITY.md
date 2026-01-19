# Attack: OS Landmines and API Compatibility

**Polecat**: metalyard/polecats/jasper
**Bead**: me-wi7
**Date**: 2026-01-19

This document attacks the Phase 1/2 design for OS-specific issues and Claude API compatibility gaps. Focus: what will break in the real world.

---

## 1. macOS + Metal Landmines

### 1.1 Unified Memory: The Design Understates the Problem

The design says:
> "Apple Silicon shares RAM between CPU and GPU. We report... `available_for_gpu`: Heuristic (75% of free RAM)"

**What will break:**

1. **75% heuristic is wrong and dangerous.** macOS aggressively manages memory pressure. When you allocate a 40GB model on a 64GB M2 Max, the system doesn't just let you have 48GB. It will:
   - Start compressing memory
   - Swap to disk (even with SSDs, this murders inference speed)
   - Kill background apps without warning
   - Eventually OOM-kill your agent if pressure continues

   The real safe number is closer to **50-60% of free RAM**, and even that varies by macOS version and what else is running.

2. **"Free RAM" is meaningless on macOS.** macOS uses all available RAM for file caching. `vm_stat` shows "free" pages, but that's not what you can allocate. You need to look at "App Memory" vs "Wired" vs "Compressed". The design doesn't specify which metric to use.

3. **Memory pressure events are not exposed via Metal.** You can't subscribe to "system is about to OOM" notifications from Metal. By the time `MTLDevice` allocation fails, it's already too late—the system is in a bad state.

**Fix needed:** Use `host_statistics64` with `HOST_VM_INFO64` to track memory pressure, not just free VRAM. Implement proactive throttling when `vm_page_free_count + vm_page_speculative_count` drops below threshold.

### 1.2 MLX Limitations

The design mentions MLX as an inference option but doesn't address:

1. **MLX model format is different from llama.cpp/GGUF.** You can't just load the same model file. MLX needs `.safetensors` or its own format. This means:
   - Double storage for models (GGUF + MLX format)
   - Or conversion pipeline (adds complexity, latency)
   - Or pick one format and lose cross-platform compatibility

2. **MLX quantization support is limited.** As of late 2025:
   - Q4_K_M and Q5_K_M (llama.cpp's best quantizations) don't exist in MLX
   - MLX has 4-bit and 8-bit, but different algorithms
   - Performance characteristics differ—what's optimal on CUDA isn't optimal on MLX

3. **MLX doesn't support all architectures.** Llama yes, but newer architectures (Mamba, RWKV, some Deepseek variants) may not work. The design assumes "llama.cpp (universal GPU support)" but that's CUDA/ROCm/Metal via llama.cpp, not MLX.

4. **llama.cpp Metal backend has its own issues:**
   - Tensor split across unified memory doesn't work the same as discrete VRAM
   - Some operations fall back to CPU silently, tanking performance
   - Metal shader compilation on first run can take 30+ seconds (design doesn't mention cold start penalty)

**Fix needed:** Specify which inference backend for macOS (llama.cpp Metal or MLX). Define model format strategy. Account for first-run shader compilation latency.

### 1.3 macOS Sandboxing and Notarization

1. **App Sandbox.** If the agent is distributed via App Store or signed for Gatekeeper, App Sandbox restrictions apply:
   - Can't access arbitrary file paths without user consent
   - Network access may require entitlements
   - GPU access is allowed but with restrictions on resource usage

2. **Notarization.** Apple scans notarized apps. If the agent downloads and executes model files:
   - Downloaded code may trigger Gatekeeper warnings
   - Dynamic library loading (for inference backends) may fail without proper signing

3. **Power management.** The design mentions "Laptop Sleeps" but on macOS:
   - `caffeinate` or `IOPMAssertionCreate` needed to prevent sleep during inference
   - Power Nap may wake the system unexpectedly
   - Hot corners / screen lock doesn't equal sleep—need to handle both

4. **Activity Monitor visibility.** Users will see GPU usage in Activity Monitor. If the agent uses 90% GPU while "idle" (preloading models), users will complain/uninstall.

**Fix needed:** Document macOS distribution strategy (signed .dmg? Homebrew? App Store?). Implement power assertion management. Define idle behavior expectations.

### 1.4 Apple Silicon Specific Gotchas

1. **M1 vs M2 vs M3 vs M4 aren't just "faster".** Each has different:
   - Neural Engine capabilities (MLX can use ANE for some ops, but not all)
   - Memory bandwidth (affects large model inference significantly)
   - Thermal envelope (M1 throttles faster than M3)

2. **MacBook vs Mac Studio vs Mac Pro.** Same M-series chip, different thermal behavior. A Mac Studio can sustain loads that will thermal-throttle a MacBook Air in 2 minutes.

3. **External displays affect GPU availability.** Driving a 6K display eats into GPU resources. The design doesn't account for this.

---

## 2. Windows-Specific Gotchas

### 2.1 Driver Hell

The design says:
> "Driver version on blacklist: Known crash/corruption bugs"

**What will break:**

1. **There is no reliable driver blacklist.** NVIDIA releases ~12 driver versions per year. Tracking which versions have which bugs for which GPUs is a full-time job. GeForce and Quadro/RTX have different driver branches with different bugs.

2. **Driver updates are uncontrollable.** Windows Update can silently update GPU drivers. User goes to bed with working agent, wakes up with broken agent because Windows pushed a new driver overnight.

3. **Multiple driver versions coexist.** Gaming-focused GeForce drivers vs Studio drivers vs Enterprise drivers. Each has different CUDA versions, different bugs, different performance.

4. **CUDA toolkit vs driver version mismatch.** If the agent bundles CUDA runtime, it may not work with all driver versions. If it relies on system CUDA, users may not have it installed.

**Fix needed:** Define CUDA version strategy (bundle runtime? require installed toolkit?). Implement driver compatibility matrix with version ranges, not blacklist. Add driver version to heartbeat for fleet-wide monitoring.

### 2.2 WSL vs Native

The design doesn't mention WSL at all, but:

1. **Many developers run inference in WSL2.** If they try to run the agent in WSL2:
   - GPU passthrough works (WSLg) but with caveats
   - NVIDIA driver in WSL is separate from Windows driver
   - File system access between WSL and Windows is slow (model loading from `/mnt/c/` is painful)
   - Network from WSL has different IP than host

2. **Docker on Windows adds another layer.** Docker Desktop on Windows uses WSL2 backend. Running agent in Docker means:
   - `--gpus all` flag needed
   - NVIDIA Container Toolkit required
   - Host networking doesn't work the same

**Fix needed:** Decide if WSL2 is supported. If yes, document GPU passthrough requirements. If no, detect and warn.

### 2.3 Antivirus and Security Software

1. **Windows Defender real-time scanning.** Model files (multi-GB) trigger scanning on first access. This adds 30-60 seconds to model loading that the design doesn't account for.

2. **Enterprise security software.** CrowdStrike, Carbon Black, etc. may:
   - Block the agent from running unsigned code
   - Throttle network connections (breaking WebSocket heartbeats)
   - Flag GPU-intensive processes as cryptominers

3. **Controlled Folder Access.** Windows security feature that prevents unknown apps from writing to Documents, Downloads, etc. Model cache location matters.

**Fix needed:** Sign the Windows executable with EV certificate. Document recommended antivirus exclusions. Use `%LOCALAPPDATA%` for cache, not user-facing folders.

### 2.4 Windows Power Management

1. **"High Performance" power plan is not default.** Many users are on "Balanced" which:
   - Throttles GPU when on battery
   - Reduces CPU frequency when "idle" (but agent is waiting for requests)
   - May spin down disks with model files

2. **Modern Standby (S0ix) is broken.** Many laptops use Modern Standby instead of S3 sleep. This means:
   - System may appear asleep but is actually running
   - Background tasks drain battery
   - Wake/sleep detection is unreliable

3. **GPU idle power states.** NVIDIA GPUs on Windows aggressively enter low-power states. First inference after idle can have 100-500ms latency penalty.

**Fix needed:** Detect and warn about non-"High Performance" power plan. Document Modern Standby behavior. Consider GPU keep-alive mechanism.

### 2.5 Multi-GPU and Display Adapter Issues

1. **Hybrid graphics (Optimus/Mux).** Gaming laptops switch between integrated and discrete GPU. Running inference on wrong GPU silently tanks performance.

2. **Multiple discrete GPUs.** The design assumes we can enumerate and use all GPUs, but:
   - SLI/NVLink configuration may present as single device
   - NVIDIA control panel "Preferred GPU" setting affects which GPU apps use
   - Some apps can't select GPU—they get whatever Windows decides

3. **GPU attached to display.** Primary display GPU has less available VRAM (frame buffer). Design says reserve 512MB headroom—this is not enough for 4K or multi-monitor setups (can be 1-2GB for frame buffers).

**Fix needed:** Detect and report hybrid graphics configuration. Warn about display-attached GPUs. Increase VRAM headroom for GPUs driving displays.

---

## 3. Linux Fragmentation

### 3.1 ROCm Support Matrix

The design says:
> "ROCm officially supports limited consumer cards. Must maintain compatibility matrix."

**What will break:**

1. **ROCm's official support matrix is small and constantly changing.** As of late 2025:
   - RX 7000 series: Partial support (7900 XT/XTX yes, 7600 unclear)
   - RX 6000 series: Some models supported
   - RX 5000 series: Dropped
   - Anything older: No

2. **"Unofficially works" is a minefield.** Community patches enable unsupported GPUs but:
   - Performance may be 50% of supported cards
   - Random crashes under load
   - No guarantee next ROCm version will work

3. **ROCm versioning is a nightmare.** ROCm 5.x vs 6.x have different:
   - Kernel module requirements
   - HSA runtime APIs
   - HIP compiler versions
   - pytorch/llama.cpp compatibility

4. **Ubuntu-centric.** ROCm installation on non-Ubuntu distros (Fedora, Arch, etc.) ranges from "difficult" to "unsupported". The design lists "Ubuntu 22.04" as example—what about users on Fedora 39 or Arch?

**Fix needed:** Define minimum ROCm version. Explicitly list supported GPU models (not "RDNA2/RDNA3"). Document installation path for non-Ubuntu distros or explicitly don't support them.

### 3.2 Distro Differences

1. **Kernel version matters.** NVIDIA DKMS modules fail to build on non-standard kernels:
   - Fedora's kernel is patched differently than Ubuntu's
   - Custom kernels (Zen, Xanmod) may not work
   - Kernel 6.x vs 5.x has different GPU subsystem behavior

2. **systemd vs non-systemd.** The design doesn't specify how the agent runs as a service. systemd unit files don't work on:
   - Gentoo (OpenRC default)
   - Void (runit)
   - Alpine (OpenRC)

3. **libc differences.** Agent binary compiled on glibc won't run on musl (Alpine). Agent binary with glibc 2.35 won't run on older distros with glibc 2.31.

4. **Wayland vs X11.** Affects:
   - How display-attached GPUs are detected
   - Screen capture for debugging (if needed)
   - NVIDIA proprietary driver behavior (worse on Wayland historically)

**Fix needed:** Define supported distros explicitly. Provide static binary or AppImage for broad compatibility. Document kernel version requirements. Support multiple init systems or document workarounds.

### 3.3 Container Environments

1. **Docker GPU access.** NVIDIA Container Toolkit has specific version requirements:
   - Host driver version must match container expectations
   - `nvidia-smi` inside container may report different values than host
   - cgroups v1 vs v2 affects GPU device access

2. **Podman (rootless).** Increasingly common on Fedora/RHEL. GPU access in rootless Podman requires:
   - CDI (Container Device Interface) configuration
   - Different from Docker's `--gpus` flag

3. **Kubernetes.** If someone tries to run agents in K8s:
   - Device plugin required
   - Resource limits may hide GPUs from container
   - Pod scheduling doesn't understand GPU capability nuances

**Fix needed:** Document container deployment path (or explicitly don't support). Test with both Docker and Podman. Mention that K8s deployment is unsupported in Phase 1.

### 3.4 Headless Servers

1. **No display manager.** On headless servers:
   - X11/Wayland isn't running
   - Some GPU detection methods fail (they expect a display)
   - NVIDIA persistence mode should be enabled

2. **SSH sessions.** Agent running in tmux/screen:
   - SIGHUP handling matters
   - Terminal size changes shouldn't crash agent
   - systemd session cleanup may kill agent on SSH disconnect

3. **Cloud GPU instances.** AWS, GCP, Azure GPU instances:
   - Different drivers (sometimes custom versions)
   - Different CUDA paths
   - Network virtualization affects latency reporting

**Fix needed:** Test headless operation explicitly. Recommend NVIDIA persistence daemon. Document cloud provider compatibility (or punt to Phase 2).

---

## 4. Claude API Compatibility Assumptions

### 4.1 Messages API Gaps

The design claims "superset of the Anthropic Messages API" but:

1. **Tool use (function calling) not mentioned.** The Anthropic API supports:
   ```json
   {
     "tools": [{"name": "calculator", "input_schema": {...}}],
     "tool_choice": {"type": "auto"}
   }
   ```
   Does the pool support this? Most open models don't support Anthropic's tool use format. This will **silently break** Claude Code workflows that use tools.

2. **System prompts.** The Messages API supports:
   ```json
   {"role": "system", "content": "You are a helpful assistant."}
   ```
   Actually wait—Anthropic uses a top-level `system` field, not a role. The design example doesn't show this. Different models handle system prompts differently. Llama-style vs ChatML-style template matters.

3. **Vision (multimodal).** Anthropic API supports:
   ```json
   {
     "content": [
       {"type": "image", "source": {"type": "base64", "data": "..."}},
       {"type": "text", "text": "What's in this image?"}
     ]
   }
   ```
   Does the pool support this? Which models? LLaVA? Not all inference backends support vision.

4. **Response format constraints.** Claude supports:
   - `response_format: {"type": "json_object"}` (not in Anthropic API but common request)
   - Grammar-constrained generation (llama.cpp supports, but API differs)

**Fix needed:** Explicitly state which Anthropic API features are supported. Tool use is critical for Claude Code—must address. Define behavior when unsupported feature is requested (error? ignore?).

### 4.2 Model Naming Mismatch

The design shows:
```
model: "claude-3-opus"     →     model: "deepseek-coder-6.7b"
```

**What will break:**

1. **Claude Code expects Claude models.** When you set `model: "deepseek-coder-6.7b"`, Claude Code doesn't know:
   - Context window size (different per model)
   - Token counting algorithm (tiktoken vs sentencepiece vs different vocab)
   - Rate limits (our pool vs Anthropic API)

2. **`model: "auto"` is underspecified.** What does "best available" mean?
   - Largest model?
   - Fastest model?
   - Most reliable agent?
   - User's preferred model?

3. **Model aliases aren't defined.** Users will try:
   - `model: "gpt-4"` (wrong API)
   - `model: "llama"` (ambiguous)
   - `model: "default"` (undefined)

**Fix needed:** Define model naming convention. Document context windows for each supported model. Define "auto" behavior explicitly. Return helpful errors for invalid model names.

### 4.3 Error Response Differences

Anthropic API errors look like:
```json
{
  "type": "error",
  "error": {
    "type": "rate_limit_error",
    "message": "Rate limit exceeded"
  }
}
```

**What will break:**

1. **Pool-specific errors.** The pool has error cases Anthropic doesn't:
   - "No capable GPU available" (capacity)
   - "Model not loaded on any agent" (model availability)
   - "All agents failed" (total failure)
   - "Queue timeout" (overload)

2. **Error type mapping.** What Anthropic error type maps to "no GPU available"?
   - `overloaded_error`? (sort of, but not the same)
   - `server_error`? (generic)
   - New type? (breaks client compatibility)

3. **Retry behavior.** Claude Code has built-in retry logic for Anthropic errors. If pool errors have different characteristics:
   - `overloaded_error` → retry with backoff (correct)
   - `invalid_request_error` → don't retry (correct)
   - `x_pool_no_capacity` → retry? how long? (undefined)

**Fix needed:** Map all pool error states to Anthropic error types. Define retry-after headers consistently. Document pool-specific error extensions in `x_pool_error` field.

### 4.4 Streaming Differences

The design shows SSE format matching Anthropic, but:

1. **Token granularity.** Anthropic streams tokens as they're generated. llama.cpp may batch tokens. Client sees different timing characteristics:
   - Anthropic: steady stream of small deltas
   - llama.cpp: bursts of tokens

2. **Content block structure.** The design shows `content_block_delta`, but what about:
   - `tool_use` blocks (if tools supported)
   - `thinking` blocks (extended thinking feature)
   - Image generation (if supported)

3. **Usage reporting.** Anthropic reports `input_tokens` and `output_tokens` in final message. Different tokenizers will give different counts. Do we report:
   - Actual tokens used by model (accurate but confusing—different from Claude)
   - Estimated "Claude-equivalent" tokens (useful but requires mapping)

4. **`message_delta` event.** Anthropic sends `message_delta` with stop_reason and usage. Design shows `message_stop` only. Missing fields will break clients expecting them.

**Fix needed:** Match Anthropic's exact SSE event sequence. Document tokenizer differences. Define `message_delta` content.

### 4.5 Rate Limiting and Quotas

The design mentions "rate limiting" but doesn't specify:

1. **Rate limit headers.** Anthropic returns:
   ```
   anthropic-ratelimit-requests-limit: 1000
   anthropic-ratelimit-requests-remaining: 999
   anthropic-ratelimit-requests-reset: 2026-01-19T22:00:00Z
   anthropic-ratelimit-tokens-limit: 100000
   anthropic-ratelimit-tokens-remaining: 99500
   anthropic-ratelimit-tokens-reset: 2026-01-19T22:00:00Z
   ```
   Does the pool return these? With what values?

2. **Token-based vs request-based limits.** Anthropic limits both. Pool might only limit one. Behavior differences will confuse users.

3. **429 response.** Standard rate limit exceeded response. Does pool use same format?

**Fix needed:** Implement Anthropic-compatible rate limit headers. Define quota model clearly.

---

## 5. Cross-Cutting Issues

### 5.1 Model Loading Time

The design says model loading takes "30-60 seconds" but:

1. **First load on macOS Metal: 30-120 seconds.** Shader compilation dominates. Subsequent loads are faster (cached).

2. **Cold storage: much longer.** If model is on HDD (common on consumer machines), loading 40GB takes 3+ minutes.

3. **VRAM fragmentation.** Loading model A, unloading, loading model B may fail even if B is smaller than A. VRAM is fragmented.

**Fix needed:** Account for cold start in routing decisions. Implement model preloading strategy. Handle VRAM fragmentation (restart inference process as workaround).

### 5.2 Network Assumptions

1. **WebSocket through corporate proxies.** Many corporate networks:
   - Block WebSocket upgrades
   - Require proxy authentication
   - Have SSL inspection that breaks certificate pinning

2. **IPv6-only networks.** Some mobile carriers and newer ISPs are IPv6-only with NAT64. Does the agent handle this?

3. **MTU issues.** Streaming large token batches may hit MTU limits on some networks. Fragmentation adds latency.

**Fix needed:** Support HTTP long-polling fallback for WebSocket. Test IPv6 connectivity. Consider message size limits.

### 5.3 Time Synchronization

1. **Clock skew.** The design uses timestamps for:
   - Heartbeat expiry
   - Ticket expiry (5 seconds)
   - Request ordering

   Consumer machines often have incorrect clocks. 5-second ticket expiry will fail if agent clock is 10 seconds off.

**Fix needed:** Use relative timeouts from server perspective, not client timestamps. Detect and warn about clock skew.

---

## Summary: Top 10 Things That Will Break

1. **macOS unified memory accounting** — 75% heuristic will cause OOMs
2. **Windows driver updates** — Silent updates break working installations
3. **ROCm on consumer AMD GPUs** — Half the cards don't work
4. **Tool use / function calling** — Not mentioned, critical for Claude Code
5. **Model format incompatibility** — GGUF vs safetensors vs different quantizations
6. **WSL2 GPU passthrough** — Common developer setup, not addressed
7. **Antivirus scanning model files** — 30-60s hidden latency
8. **Non-Ubuntu Linux distros** — ROCm barely works outside Ubuntu
9. **Streaming event format differences** — Missing `message_delta`, different timing
10. **Rate limit header compatibility** — Clients expect Anthropic's headers

---

## Recommendations

1. **Narrow scope for Phase 1**: Support NVIDIA on Linux and Windows only. macOS and AMD are Phase 2.

2. **Define Claude Code compatibility explicitly**: Which features work, which don't, what errors to expect.

3. **Build OS detection into agent**: Report detailed system info in registration for debugging fleet issues.

4. **Create compatibility test suite**: Automated tests that verify API compatibility against real Anthropic API behavior.

5. **Accept that "just works" is impossible**: Consumer hardware is a jungle. Documentation and error messages matter more than trying to handle every case automatically.
