package platform

import (
	"context"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/mem"
)

// Stats is what the dashboard reports about the machine.
//
// Every field is a pointer because "unknown" and "zero" are different answers.
// A machine with no GPU and a machine whose GPU query failed should not render
// identically, and a UI that has to guess which one it is looking at will guess
// wrong.
type Stats struct {
	CPU    CPUStats    `json:"cpu"`
	Memory MemoryStats `json:"memory"`
	Disk   DiskStats   `json:"disk"`

	// GPU is empty rather than null when there is none, so the dashboard
	// renders one shape everywhere.
	GPU []GPUInfo `json:"gpu"`
}

// CPUStats is processor load.
type CPUStats struct {
	Cores        int      `json:"cores"`
	UsagePercent *float64 `json:"usage_percent"`
}

// MemoryStats is physical memory.
type MemoryStats struct {
	TotalBytes *int64 `json:"total_bytes"`
	UsedBytes  *int64 `json:"used_bytes"`
}

// DiskStats is free space at the two paths that matter.
type DiskStats struct {
	DataFreeBytes   *int64 `json:"data_free_bytes"`
	ModelsFreeBytes *int64 `json:"models_free_bytes"`
}

// GPUInfo is one detected accelerator.
type GPUInfo struct {
	Index              int    `json:"index"`
	Name               string `json:"name"`
	MemoryTotalBytes   *int64 `json:"memory_total_bytes,omitempty"`
	MemoryUsedBytes    *int64 `json:"memory_used_bytes,omitempty"`
	UtilizationPercent *int   `json:"utilization_percent,omitempty"`
	Driver             string `json:"driver,omitempty"`
	CUDA               string `json:"cuda,omitempty"`
}

// HostStats gathers what the dashboard shows.
//
// Nothing here is fatal. A machine where the memory query fails still has a
// usable dashboard, and failing the whole request because one figure is
// unavailable would take away the four that are not.
func HostStats(ctx context.Context, dataDir, modelDir string) Stats {
	stats := Stats{
		CPU: CPUStats{Cores: runtime.NumCPU()},
		GPU: []GPUInfo{},
	}

	// Sampled over a short interval rather than read instantaneously: the first
	// reading from gopsutil is the average since boot, which on a machine that
	// has been up for a week says nothing about right now.
	if percents, err := cpu.PercentWithContext(ctx, 200*time.Millisecond, false); err == nil && len(percents) > 0 {
		usage := percents[0]
		stats.CPU.UsagePercent = &usage
	}

	if memory, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		total := int64(memory.Total)
		used := int64(memory.Used)
		stats.Memory.TotalBytes = &total
		stats.Memory.UsedBytes = &used
	}

	stats.Disk.DataFreeBytes = FreeSpace(dataDir)
	stats.Disk.ModelsFreeBytes = FreeSpace(modelDir)

	stats.GPU = GPUs(ctx)

	return stats
}

// FreeSpace reports the bytes available on the filesystem holding path.
//
// gopsutil rather than a platform-specific call: the implementations it wraps
// are each easy to get subtly wrong — `statfs` field units differ between Linux
// and macOS, and Windows needs a different call entirely — and a wrong answer
// produces a confusing disk-full failure much later.
//
// A nil return means "could not tell", which the API renders as null.
func FreeSpace(path string) *int64 {
	if path == "" {
		return nil
	}
	usage, err := disk.Usage(path)
	if err != nil {
		return nil
	}
	free := int64(usage.Free)
	return &free
}

// ---------------------------------------------------------------------------
// GPU
// ---------------------------------------------------------------------------

// gpuProbeTimeout bounds the accelerator query.
//
// Short, because this runs while a page is loading and nvidia-smi is a separate
// process. A machine whose driver is wedged should produce a dashboard without a
// GPU section, not a dashboard that never arrives.
const gpuProbeTimeout = 2 * time.Second

// GPUs reports the NVIDIA accelerators this machine exposes.
//
// NVIDIA only, queried through nvidia-smi. It is the accelerator that matters
// here — faster-whisper runs on CUDA or on the CPU, and this project does not
// target other vendors — and reading it from the vendor's own tool is the one
// answer that cannot disagree with what the inference library will find.
//
// An absent nvidia-smi is not an error: it is the normal state of a machine
// without an NVIDIA card.
//
// Exported because the install decision needs this answer on its own. The
// System page reads it as part of HostStats, but an install request has to ask
// "is the CUDA set worth downloading" without also sampling the CPU for 200 ms
// and stat-ing two filesystems.
func GPUs(ctx context.Context) []GPUInfo {
	path, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return []GPUInfo{}
	}

	ctx, cancel := context.WithTimeout(ctx, gpuProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path,
		"--query-gpu=index,name,memory.total,memory.used,utilization.gpu,driver_version",
		"--format=csv,noheader,nounits")
	cmd.Stdin = nil

	output, err := cmd.Output()
	if err != nil {
		// The tool exists but would not answer — a driver mismatch, a container
		// without the device. Reported as no GPUs, which is what the inference
		// library will also conclude.
		return []GPUInfo{}
	}

	cudaVersion := queryCUDAVersion(ctx, path)

	var gpus []GPUInfo
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			continue
		}
		for i := range fields {
			fields[i] = strings.TrimSpace(fields[i])
		}

		gpu := GPUInfo{
			Name:   fields[1],
			Driver: fields[5],
			CUDA:   cudaVersion,
		}
		if index, err := strconv.Atoi(fields[0]); err == nil {
			gpu.Index = index
		}
		// [N/A] is what nvidia-smi prints for a figure it cannot read, which is
		// common in a container that was not given the device.
		if total, err := strconv.ParseInt(fields[2], 10, 64); err == nil {
			totalBytes := total * 1024 * 1024
			gpu.MemoryTotalBytes = &totalBytes
		}
		if used, err := strconv.ParseInt(fields[3], 10, 64); err == nil {
			usedBytes := used * 1024 * 1024
			gpu.MemoryUsedBytes = &usedBytes
		}
		if utilization, err := strconv.Atoi(fields[4]); err == nil {
			gpu.UtilizationPercent = &utilization
		}

		gpus = append(gpus, gpu)
	}

	if gpus == nil {
		return []GPUInfo{}
	}
	return gpus
}

// queryCUDAVersion reads the CUDA version the driver supports.
func queryCUDAVersion(ctx context.Context, smi string) string {
	cmd := exec.CommandContext(ctx, smi, "--query-gpu=driver_version", "--format=csv,noheader")
	cmd.Stdin = nil
	if _, err := cmd.Output(); err != nil {
		return ""
	}

	// The CUDA version is not a queryable field on older drivers, so it comes
	// from the header block of the plain invocation. Best effort: an empty
	// string means "not reported", which the UI omits.
	full := exec.CommandContext(ctx, smi)
	full.Stdin = nil
	output, err := full.Output()
	if err != nil {
		return ""
	}

	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "CUDA Version:") {
			_, after, found := strings.Cut(line, "CUDA Version:")
			if !found {
				continue
			}
			return strings.TrimSpace(strings.Split(after, "|")[0])
		}
	}
	return ""
}
