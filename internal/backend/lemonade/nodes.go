package lemonade

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	// BudgetSourceSystemInfo marks a node budget taken from the host memory
	// Lemonade Server reports in GET /api/v1/system-info.
	BudgetSourceSystemInfo = "system-info"

	// InventoryModels marks a cache block computed from the backend's own
	// model list rather than a scan of the store.
	InventoryModels = "models"

	// hostNodeMessage explains the host node's numbers to API clients.
	hostNodeMessage = "host memory and accelerators as Lemonade Server reports them (/api/v1/system-info); reservation is the catalog size of the loaded models; the cache is Lemonade's model store"
)

// ListNodes implements backend.NodeLister: the proxied host is the one node,
// described by Lemonade's own system-info — the host's memory as the budget
// (BudgetSourceSystemInfo), the accelerators Lemonade enumerates (the NPU,
// the GPUs) as gpuCount/gpuProduct, what the loaded models reserve (their
// catalog sizes; Lemonade reports no per-model memory) and the model store
// as the node's cache.
func (b *Backend) ListNodes(ctx context.Context) ([]backend.NodeInfo, error) {
	info, err := b.client.SystemInfo(ctx)
	if err != nil {
		return nil, err
	}
	models, err := b.client.Models(ctx)
	if err != nil {
		return nil, err
	}
	health, err := b.client.Health(ctx)
	if err != nil {
		return nil, err
	}
	node := backend.NodeInfo{
		Name:         hostOf(b.agentHost),
		Ready:        true,
		Eligible:     true, // the proxied host is the only serving target
		BudgetSource: BudgetSourceSystemInfo,
		Message:      hostNodeMessage,
	}
	if info.Devices.CPU != nil {
		node.Architecture = normalizeArch(info.Devices.CPU.Family)
	}
	total, err := parseMemory(info.PhysicalMemory)
	if err != nil {
		node.Message = fmt.Sprintf("host memory unknown (%v); %s", err, hostNodeMessage)
	} else {
		node.AllocatableMemoryBytes = total
		node.BudgetBytes = total
	}
	node.GPUCount, node.GPUProduct = accelerators(info)

	sizes := make(map[string]int64, len(models))
	downloaded := 0
	var cacheBytes int64
	for _, m := range models {
		if !m.downloaded() {
			continue
		}
		downloaded++
		sizes[m.ID] = gbToBytes(m.SizeGB)
		cacheBytes += sizes[m.ID]
	}
	for _, l := range health.AllModelsLoaded {
		node.ReservedBytes += sizes[l.ModelName]
	}
	node.FreeBytes = node.BudgetBytes - node.ReservedBytes
	if node.FreeBytes < 0 {
		node.FreeBytes = 0
	}
	if info.ModelStorage != nil && info.ModelStorage.Path != "" {
		node.Cache = &backend.NodeCache{
			MountPath: info.ModelStorage.Path,
			Models:    downloaded,
			BytesUsed: cacheBytes,
			ScannedAt: b.now(),
			Inventory: InventoryModels,
		}
	}
	return []backend.NodeInfo{node}, nil
}

// accelerators counts the devices Lemonade found available — the NPU and
// the GPUs — and names the NPU when there is one, else the first GPU.
func accelerators(info *systemInfo) (int64, string) {
	var count int64
	product := ""
	if npu := info.Devices.AMDNPU; npu != nil && npu.Available {
		count++
		product = npu.Name
	}
	for _, gpus := range [][]device{info.Devices.AMDGPU, info.Devices.NvidiaGPU} {
		for _, g := range gpus {
			if !g.Available {
				continue
			}
			count++
			if product == "" {
				product = g.Name
			}
		}
	}
	return count, product
}

// parseMemory reads Lemonade's "Physical Memory" figure ("86.07 GB") into
// bytes. Lemonade labels binary units GB: on a host whose /proc/meminfo has
// MemTotal 90251884 kB (86.07 GiB) it prints 86.07 GB, so every unit is read
// as its binary size.
func parseMemory(raw string) (int64, error) {
	fields := strings.Fields(strings.TrimSpace(raw))
	if len(fields) == 0 || len(fields) > 2 {
		return 0, fmt.Errorf("malformed memory figure %q", raw)
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return 0, fmt.Errorf("malformed memory figure %q", raw)
	}
	unit := "gb"
	if len(fields) == 2 {
		unit = strings.ToLower(fields[1])
	}
	var scale float64
	switch unit {
	case "b", "bytes":
		scale = 1
	case "kb", "kib":
		scale = 1 << 10
	case "mb", "mib":
		scale = 1 << 20
	case "gb", "gib":
		scale = 1 << 30
	case "tb", "tib":
		scale = 1 << 40
	default:
		return 0, fmt.Errorf("unknown unit in memory figure %q", raw)
	}
	return int64(math.Round(v * scale)), nil
}

// normalizeArch maps Lemonade's CPU family onto the architecture names
// Kubernetes nodes report.
func normalizeArch(family string) string {
	switch f := strings.ToLower(strings.TrimSpace(family)); f {
	case "x86_64", "amd64", "x64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return f
	}
}

// hostOf returns the host part of an address agents dial (the agent host as
// written into ModelConfigs, e.g. http://172.21.0.1:13305 -> 172.21.0.1). An
// address without a scheme is read as host[:port]; anything unparsable is
// returned as is.
func hostOf(addr string) string {
	s := strings.TrimSpace(addr)
	if !strings.Contains(s, "://") {
		s = "http://" + s
	}
	if u, err := url.Parse(s); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return strings.TrimSpace(addr)
}
