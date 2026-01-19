package main

import (
	"bufio"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// GPUInfo represents detected GPU information
type GPUInfo struct {
	ID         string `json:"id"`          // "cuda:0", "metal:0", "cpu"
	Type       string `json:"type"`        // "nvidia", "apple", "cpu"
	Name       string `json:"name"`        // "RTX 4090", "M2 Max", "CPU"
	VRAM_MB    int    `json:"vram_mb"`     // Available VRAM (0 for unified/CPU)
	ComputeCap string `json:"compute_cap"` // "8.9" for NVIDIA, "apple3" for Metal, "" for CPU
}

// DetectGPUs detects available GPUs using llama-server
func DetectGPUs() ([]GPUInfo, error) {
	// Try llama-server first (preferred method)
	gpus, err := detectViaLlamaServer()
	if err == nil && len(gpus) > 0 {
		return gpus, nil
	}

	// Fallback to platform-specific detection
	switch runtime.GOOS {
	case "darwin":
		return detectAppleSilicon()
	case "linux", "windows":
		return detectNVIDIA()
	default:
		return detectCPUOnly()
	}
}

// detectViaLlamaServer uses llama-server --list-gpus
func detectViaLlamaServer() ([]GPUInfo, error) {
	cmd := exec.Command("llama-server", "--list-gpus")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("llama-server not found or failed: %w", err)
	}

	return parseLlamaServerOutput(string(output))
}

// parseLlamaServerOutput parses llama-server --list-gpus output
func parseLlamaServerOutput(output string) ([]GPUInfo, error) {
	var gpus []GPUInfo
	scanner := bufio.NewScanner(strings.NewReader(output))

	// Expected format varies by llama.cpp version
	// Common patterns:
	// "GPU 0: NVIDIA GeForce RTX 4090 (24576 MB)"
	// "Metal: Apple M2 Max"
	gpuPattern := regexp.MustCompile(`(?i)GPU\s+(\d+):\s+(.+?)\s*\((\d+)\s*MB\)`)
	metalPattern := regexp.MustCompile(`(?i)Metal:\s+(.+)`)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if matches := gpuPattern.FindStringSubmatch(line); matches != nil {
			idx := matches[1]
			name := strings.TrimSpace(matches[2])
			vram, _ := strconv.Atoi(matches[3])

			gpu := GPUInfo{
				ID:      fmt.Sprintf("cuda:%s", idx),
				Type:    "nvidia",
				Name:    name,
				VRAM_MB: vram,
			}

			// Extract compute capability if present in name
			if ccMatch := regexp.MustCompile(`CC\s*(\d+\.\d+)`).FindStringSubmatch(name); ccMatch != nil {
				gpu.ComputeCap = ccMatch[1]
			}

			gpus = append(gpus, gpu)
		} else if matches := metalPattern.FindStringSubmatch(line); matches != nil {
			gpus = append(gpus, GPUInfo{
				ID:         "metal:0",
				Type:       "apple",
				Name:       strings.TrimSpace(matches[1]),
				VRAM_MB:    0, // Unified memory
				ComputeCap: "apple3",
			})
		}
	}

	return gpus, nil
}

// detectNVIDIA uses nvidia-smi as fallback
func detectNVIDIA() ([]GPUInfo, error) {
	cmd := exec.Command("nvidia-smi",
		"--query-gpu=index,name,memory.total,compute_cap",
		"--format=csv,noheader,nounits")
	output, err := cmd.Output()
	if err != nil {
		return detectCPUOnly()
	}

	var gpus []GPUInfo
	scanner := bufio.NewScanner(strings.NewReader(string(output)))

	for scanner.Scan() {
		parts := strings.Split(scanner.Text(), ", ")
		if len(parts) < 4 {
			continue
		}

		idx := strings.TrimSpace(parts[0])
		name := strings.TrimSpace(parts[1])
		vram, _ := strconv.Atoi(strings.TrimSpace(parts[2]))
		cc := strings.TrimSpace(parts[3])

		gpus = append(gpus, GPUInfo{
			ID:         fmt.Sprintf("cuda:%s", idx),
			Type:       "nvidia",
			Name:       name,
			VRAM_MB:    vram,
			ComputeCap: cc,
		})
	}

	if len(gpus) == 0 {
		return detectCPUOnly()
	}

	return gpus, nil
}

// detectAppleSilicon detects Apple Silicon GPU
func detectAppleSilicon() ([]GPUInfo, error) {
	cmd := exec.Command("system_profiler", "SPDisplaysDataType")
	output, err := cmd.Output()
	if err != nil {
		return detectCPUOnly()
	}

	// Parse system_profiler output for chip name
	chipPattern := regexp.MustCompile(`Chip Model:\s+(.+)`)
	if matches := chipPattern.FindStringSubmatch(string(output)); matches != nil {
		return []GPUInfo{{
			ID:         "metal:0",
			Type:       "apple",
			Name:       strings.TrimSpace(matches[1]),
			VRAM_MB:    0, // Unified memory - actual available depends on system RAM
			ComputeCap: "apple3",
		}}, nil
	}

	return detectCPUOnly()
}

// detectCPUOnly returns CPU-only fallback
func detectCPUOnly() ([]GPUInfo, error) {
	return []GPUInfo{{
		ID:         "cpu",
		Type:       "cpu",
		Name:       "CPU",
		VRAM_MB:    0,
		ComputeCap: "",
	}}, nil
}

// CanServe checks if GPU can serve a model with given VRAM requirement
func (g GPUInfo) CanServe(modelVRAM_MB int) bool {
	if g.Type == "cpu" {
		return true // CPU always works (slowly)
	}
	if g.Type == "apple" {
		return true // Unified memory, assume it works
	}
	// For discrete GPUs, check VRAM with headroom
	if g.VRAM_MB < modelVRAM_MB+512 {
		return false
	}
	// Check compute capability for NVIDIA
	if g.Type == "nvidia" && g.ComputeCap != "" {
		cc, _ := strconv.ParseFloat(g.ComputeCap, 64)
		if cc < 7.0 {
			return false // Pre-Volta too slow
		}
	}
	return true
}

// String returns a human-readable description
func (g GPUInfo) String() string {
	if g.Type == "cpu" {
		return "CPU (fallback)"
	}
	if g.Type == "apple" {
		return fmt.Sprintf("%s (Metal)", g.Name)
	}
	return fmt.Sprintf("%s (%d MB, CC %s)", g.Name, g.VRAM_MB, g.ComputeCap)
}
