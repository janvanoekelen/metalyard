# GPU Detection & Hardware Capability Evaluation Design

**Author**: Albert (metalyard/crew/Albert)
**Parent Issue**: me-xwd (Phase 1: Design distributed GPU pooling architecture)
**Date**: 2026-01-19

---

## 1. Platform-Specific GPU Detection

### 1.1 Detection Strategy Overview

The desktop agent must detect available GPUs across heterogeneous hardware. We use a **layered detection approach**: try vendor-specific APIs first (most accurate), fall back to OS-generic APIs, then to basic enumeration.

```
┌─────────────────────────────────────────────────────────┐
│                    Detection Pipeline                    │
├─────────────────────────────────────────────────────────┤
│  Layer 1: Vendor APIs (nvml, rocm-smi, Metal)           │
│  Layer 2: OS APIs (DXGI, sysfs, IOKit)                  │
│  Layer 3: Generic fallback (OpenCL, Vulkan enumeration) │
│  Layer 4: CPU-only mode                                  │
└─────────────────────────────────────────────────────────┘
```

### 1.2 Windows Detection

**Primary: NVIDIA (nvml)**
```
nvmlInit() → nvmlDeviceGetCount() → nvmlDeviceGetHandleByIndex()
  → nvmlDeviceGetName()
  → nvmlDeviceGetMemoryInfo() → total, free, used
  → nvmlDeviceGetCudaComputeCapability() → major.minor
  → nvmlDeviceGetPowerManagementLimit()
  → nvmlDeviceGetTemperatureThreshold()
```

**Primary: AMD (WMI + ADL)**
- WMI: `Win32_VideoController` for basic enumeration
- ADL (AMD Display Library): Detailed VRAM, clocks, thermals
- ROCm-SMI if ROCm installed (preferred for compute)

**Fallback: DXGI**
```cpp
IDXGIFactory1::EnumAdapters1() → IDXGIAdapter1
  → GetDesc1() → Description, DedicatedVideoMemory, VendorId
```
- Catches all GPUs including Intel integrated
- Less detailed than vendor APIs but universally available

**Detection Order (Windows)**:
1. Try nvml (NVIDIA)
2. Try ADL/ROCm-SMI (AMD)
3. Fall back to DXGI for any missed GPUs
4. Cross-reference to avoid duplicates (match by PCI bus ID)

### 1.3 Linux Detection

**Primary: NVIDIA (nvml)**
- Same API as Windows, linked against `libnvidia-ml.so`
- Requires NVIDIA driver installed

**Primary: AMD (sysfs + ROCm-SMI)**
```bash
# sysfs enumeration
/sys/class/drm/card*/device/vendor    # 0x1002 = AMD
/sys/class/drm/card*/device/device    # Device ID
/sys/class/drm/card*/device/mem_info_vram_total
/sys/class/drm/card*/device/gpu_busy_percent
```

ROCm-SMI provides structured data:
```
rocm-smi --showmeminfo vram --json
rocm-smi --showproductname --json
```

**Fallback: libdrm**
- `drmGetDevices2()` enumerates all DRM devices
- Parse sysfs for detailed info

**Intel (for completeness)**
- Usually integrated, rarely useful for LLM inference
- Detect via `/sys/class/drm/card*/device/vendor` (0x8086)
- Intel Arc discrete GPUs: Use Level Zero API or Vulkan

**Detection Order (Linux)**:
1. Try nvml (NVIDIA)
2. Scan sysfs for AMD (vendor 0x1002)
3. Use ROCm-SMI if available for detailed AMD info
4. libdrm fallback for anything missed

### 1.4 macOS Detection

**Apple Silicon (M1/M2/M3/M4 series)**
- No discrete GPU, unified memory architecture
- Detection via IOKit + Metal

```objc
// Metal device enumeration
MTLCopyAllDevices() → [MTLDevice]
  → device.name                    // "Apple M2 Pro"
  → device.recommendedMaxWorkingSetSize  // Usable for ML
  → device.hasUnifiedMemory        // Always true on Apple Silicon
```

**IOKit for system info**:
```
IOServiceMatching("AppleARMIODevice")
→ "gpu-core-count"
→ "gpu-memory-size" (from system total, shared)
```

**Memory consideration**: Apple Silicon shares RAM between CPU and GPU. We report:
- `total_unified_memory`: System RAM
- `available_for_gpu`: Heuristic (total - OS overhead - app usage), typically 75% of free RAM

**Intel Macs (legacy)**
- Discrete AMD GPUs: IOKit + Metal
- Integrated Intel: Usually not useful for inference

### 1.5 CPU Fallback

When no suitable GPU is detected:
```
Detection: Always available
Capabilities:
  - CPU model (lscpu, sysctl, CPUID)
  - Core count (physical + logical)
  - RAM available
  - AVX/AVX2/AVX-512 support (critical for llama.cpp CPU inference)
  - NUMA topology (for large systems)
```

---

## 2. Model Capability Evaluation

### 2.1 VRAM Thresholds

LLM memory requirements depend on model size and quantization:

| Model Size | Q4_K_M | Q5_K_M | Q8_0 | FP16 |
|------------|--------|--------|------|------|
| 7B         | 4.4 GB | 5.1 GB | 7.5 GB | 14 GB |
| 13B        | 7.9 GB | 9.1 GB | 14 GB | 26 GB |
| 34B        | 20 GB  | 23 GB  | 36 GB | 68 GB |
| 70B        | 40 GB  | 46 GB  | 75 GB | 140 GB |

**Evaluation logic**:
```python
def can_run_model(gpu_vram_gb, model_size_b, quant):
    required = MODEL_REQUIREMENTS[(model_size_b, quant)]
    # Leave 10% headroom for KV cache and runtime overhead
    return gpu_vram_gb * 0.9 >= required
```

**Multi-GPU consideration**:
- For tensor parallelism: sum VRAM across identical GPUs
- For pipeline parallelism: minimum VRAM determines layer capacity
- Mixed GPU configs: Complex, initially unsupported

### 2.2 Compute Capability Requirements

**NVIDIA CUDA Compute Capability**:
| CC | Architecture | Status |
|----|--------------|--------|
| < 6.0 | Maxwell/older | REJECT - No FP16 |
| 6.x | Pascal | MINIMUM - Basic FP16 |
| 7.x | Volta/Turing | GOOD - Tensor cores (7.0+) |
| 8.x | Ampere | EXCELLENT - BF16, sparse |
| 9.x | Hopper | OPTIMAL - FP8, transformer engine |

**AMD ROCm Support**:
- RDNA2/RDNA3: Consumer cards, limited ROCm support
- CDNA (MI100/MI200/MI300): Full ROCm, excellent for inference
- Check `rocminfo` for gfx version (gfx1030 = RDNA2, gfx90a = MI200)

**Apple Silicon**:
- All M-series support Metal and MLX
- M1: 8 GPU cores (base), usable for 7B
- M1 Pro/Max/Ultra: 16-64 cores, usable for 13B-70B
- M2/M3/M4: Improved ML performance, similar memory constraints

### 2.3 Disqualification Criteria

A GPU is **disqualified** if ANY of these are true:

```python
DISQUALIFY_REASONS = {
    "vram_too_low": vram_gb < 4,  # Can't run smallest useful models
    "compute_too_old": nvidia_cc < 6.0,
    "driver_missing": not has_working_driver(),
    "driver_too_old": driver_version < minimum_required,
    "rocm_unsupported": is_amd and gfx_version not in SUPPORTED_GFX,
    "thermal_throttled": current_temp > throttle_threshold,
    "insufficient_power": power_limit < minimum_watts,
    "in_use_exclusive": gpu_utilization > 95,  # Already saturated
}
```

**Soft disqualification** (warn but allow):
- VRAM 4-6 GB: Only smallest models
- Shared/integrated GPU: Performance will be poor
- Laptop GPU: May thermal throttle under sustained load

---

## 3. Driver Detection and Validation

### 3.1 Driver Version Requirements

**NVIDIA**:
```
Minimum: 525.x (CUDA 12.0 support)
Recommended: 535.x+ (better stability, bug fixes)

Detection (nvml):
  nvmlSystemGetDriverVersion() → "535.104.05"
  nvmlSystemGetCudaDriverVersion() → 12020 (12.2)
```

**AMD ROCm**:
```
Minimum: ROCm 5.4 (reasonable LLM support)
Recommended: ROCm 5.7+ (better performance)

Detection:
  rocm-smi --showdriverversion
  /opt/rocm/.info/version
```

**Apple Metal**:
```
Minimum: macOS 12.0 (Monterey)
Recommended: macOS 14.0+ (MLX optimizations)

Detection:
  MTLDevice.supportsFamily(.metal3)
  sw_vers -productVersion
```

### 3.2 Driver Health Checks

Beyond version, validate driver is functional:

```python
def validate_driver(gpu):
    checks = [
        ("initialization", try_init_context()),
        ("memory_alloc", try_allocate_test_buffer()),
        ("compute", try_simple_kernel()),
        ("memory_transfer", try_memcpy_roundtrip()),
    ]

    for name, result in checks:
        if not result.success:
            return DriverStatus.BROKEN, f"Failed: {name} - {result.error}"

    return DriverStatus.HEALTHY, None
```

### 3.3 Common Driver Issues

| Issue | Detection | Action |
|-------|-----------|--------|
| Driver not installed | nvml init fails | Guide user to install |
| Driver/kernel mismatch | Init succeeds but ops fail | Suggest reboot or reinstall |
| GPU in use by X11/Wayland | Low free VRAM on idle | Warn about reduced capacity |
| Secure boot blocking | Driver loads but no GPU | Detect and warn |
| Container missing --gpus | No devices visible | Detect container, guide user |

---

## 4. Detection Output Schema

```json
{
  "detection_timestamp": "2026-01-19T21:30:00Z",
  "platform": "linux",
  "gpus": [
    {
      "id": "gpu-0",
      "name": "NVIDIA RTX 4090",
      "vendor": "nvidia",
      "pci_bus_id": "0000:01:00.0",
      "vram_total_gb": 24.0,
      "vram_free_gb": 23.1,
      "compute_capability": "8.9",
      "driver_version": "535.104.05",
      "cuda_version": "12.2",
      "status": "available",
      "capabilities": {
        "fp16": true,
        "bf16": true,
        "int8": true,
        "tensor_cores": true
      },
      "max_model_size": {
        "q4_k_m": "70B",
        "q8_0": "34B",
        "fp16": "13B"
      },
      "health": {
        "temperature_c": 42,
        "power_draw_w": 25,
        "utilization_pct": 0
      }
    }
  ],
  "cpu_fallback": {
    "model": "AMD Ryzen 9 7950X",
    "cores": 16,
    "threads": 32,
    "ram_total_gb": 64,
    "ram_free_gb": 58,
    "avx512": true,
    "status": "available"
  },
  "recommended_backend": "cuda",
  "warnings": []
}
```

---

## 5. Non-Obvious Design Decisions

### 5.1 Why Layered Detection (Not Just Vulkan/OpenCL)?

**Decision**: Use vendor-specific APIs first, generic APIs as fallback.

**Rationale**:
- Vulkan/OpenCL don't expose VRAM accurately on all platforms
- No compute capability info from generic APIs
- Driver version/health not available
- Vendor APIs give thermal/power data needed for scheduling

**Trade-off**: More code paths, but much better accuracy for capacity planning.

### 5.2 Why Conservative VRAM Estimates?

**Decision**: Use 90% of reported VRAM as "available" for model loading.

**Rationale**:
- OS/driver reserve memory
- KV cache grows during inference
- Multiple requests need headroom
- Better to under-promise than OOM

### 5.3 Why Disqualify Old NVIDIA GPUs (CC < 6.0)?

**Decision**: Hard reject GPUs older than Pascal.

**Rationale**:
- No native FP16, inference is painfully slow
- Drivers increasingly unsupported
- VRAM typically < 8GB anyway
- Maintenance burden not worth edge cases

### 5.4 Why Treat Apple Silicon Specially?

**Decision**: Unified memory requires different accounting than discrete GPUs.

**Rationale**:
- No separate VRAM to measure
- System pressure affects GPU availability
- MLX/Metal have different optimal quantizations
- Can't just sum "GPU memory" like discrete cards

### 5.5 Why Include CPU Fallback in GPU Detection?

**Decision**: Always report CPU as a fallback compute option.

**Rationale**:
- llama.cpp CPU inference is viable for small models
- Provides graceful degradation
- Some users have powerful CPUs but no GPU
- Enables hybrid CPU+GPU inference strategies

---

## 6. Implementation Recommendations

1. **Single detection library**: Create `gpu-detect` module used by all agents
2. **Periodic refresh**: Re-detect every 60s to catch thermal/availability changes
3. **Cache aggressively**: Full detection is expensive, cache static info
4. **Graceful degradation**: Never crash on detection failure, fall back to CPU
5. **Structured logging**: Log all detection results for debugging fleet issues

---

## Next Steps

This design should integrate with:
- **Heartbeat/registration**: Include GPU info in agent heartbeat
- **Scheduler**: Use `max_model_size` for routing decisions
- **Health monitoring**: Track thermal/utilization trends
