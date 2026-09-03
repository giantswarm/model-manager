package ollama

import (
	"bufio"
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	// procMeminfo is where the driver reads the host's memory total. A pod
	// sees the kernel's figure: on kind (the lab) and on any install where
	// Ollama and the pod share the machine's kernel that is the machine's RAM;
	// under a VM-backed container runtime (Docker Desktop, Podman machine) it
	// is the VM's RAM, not the machine's.
	procMeminfo = "/proc/meminfo"

	// BudgetSourceHostMeminfo marks a node budget taken from MemTotal of
	// /proc/meminfo as the model-manager pod sees it.
	BudgetSourceHostMeminfo = "host-meminfo"

	// BudgetSourceOverride marks a node budget set by the operator
	// (ollama.memoryBudgetGiB / --ollama-memory-budget-gib) instead of the
	// pod's MemTotal: the ollama counterpart of the kserve node annotation
	// model-manager.giantswarm.io/memory-budget-gib, for installs where the
	// pod's view of the host memory is wrong (a VM-backed container runtime,
	// an Ollama on another machine).
	BudgetSourceOverride = "override"

	// overrideKnob names the override for messages: the chart value and the
	// flag behind it.
	overrideKnob = "ollama.memoryBudgetGiB / --ollama-memory-budget-gib"

	// hostNodeMessage explains the host node's numbers to API clients.
	hostNodeMessage = "host memory as seen from the model-manager pod; per-model accelerator share in running.vramBytes"

	// overrideMessage explains the host node's numbers under an override.
	overrideMessage = "memory budget set by the operator (" + overrideKnob + "), not the host's MemTotal; per-model accelerator share in running.vramBytes"

	gib = int64(1 << 30)
)

// ListNodes implements backend.NodeLister: the proxied host is the one node.
// Ollama's API has no capacity endpoint (the totals appear only in its startup
// log), so the budget is MemTotal of /proc/meminfo as the pod sees it — on the
// unified-memory laptops Ollama typically runs on, GPU memory is system memory
// anyway — unless the operator set a budget (BudgetSourceOverride), and what
// loaded models reserve is the sum of `size` over /api/ps. AllocatableMemoryBytes
// stays the pod's MemTotal either way, as the kserve node's allocatable memory
// stays under its annotation.
// Accelerated says whether any loaded model has memory on an accelerator
// (`size_vram` > 0); the accelerator itself is not observable through Ollama's
// API, so there is no GPU count, product or memory and no cache block.
func (b *Backend) ListNodes(ctx context.Context) ([]backend.NodeInfo, error) {
	ps, err := b.client.PS(ctx)
	if err != nil {
		return nil, err
	}
	node := backend.NodeInfo{
		Name:         hostOf(b.agentHost),
		Ready:        true,
		Eligible:     true, // the proxied host is the only serving target
		Architecture: runtime.GOARCH,
		BudgetSource: BudgetSourceHostMeminfo,
		Message:      hostNodeMessage,
	}
	accelerated := false
	for _, p := range ps {
		node.ReservedBytes += p.Size
		if p.SizeVRAM > 0 {
			accelerated = true
		}
	}
	node.Accelerated = &accelerated
	explain := hostNodeMessage
	override, ignored := budgetOverride(b.memoryBudgetGiB)
	if override > 0 {
		node.BudgetBytes = override
		node.BudgetSource = BudgetSourceOverride
		explain = overrideMessage
	}
	total, err := readMemTotal(b.meminfoPath)
	if err != nil {
		node.Message = fmt.Sprintf("host memory unknown (%v); %s", err, explain)
	} else {
		node.AllocatableMemoryBytes = total
		if override == 0 {
			node.BudgetBytes = total
		}
		node.Message = explain
	}
	if ignored != "" {
		node.Message = ignored + "; " + node.Message
	}
	node.FreeBytes = node.BudgetBytes - node.ReservedBytes
	if node.FreeBytes < 0 {
		node.FreeBytes = 0
	}
	return []backend.NodeInfo{node}, nil
}

// budgetOverride parses the operator's memory budget override, in GiB with
// decimals allowed, into bytes. Empty or 0 means none (0, ""). Anything else
// that is not a positive finite number is ignored — same rule as the kserve
// budget annotation — and the reason is returned for the node's message.
func budgetOverride(raw string) (int64, string) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return 0, ""
	}
	v, err := strconv.ParseFloat(s, 64)
	if err == nil && v == 0 {
		return 0, ""
	}
	bytes := int64(0)
	if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v > 0 {
		bytes = int64(math.Round(v * float64(gib)))
	}
	if bytes <= 0 {
		return 0, fmt.Sprintf("ignoring memory budget override %q (%s): want a positive number of GiB; budget taken from %s", raw, overrideKnob, BudgetSourceHostMeminfo)
	}
	return bytes, ""
}

// hostOf returns the host part of an address agents dial (the agent host as
// written into ModelConfigs, e.g. http://172.21.0.1:11434 -> 172.21.0.1). An
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

// readMemTotal returns MemTotal of a /proc/meminfo-style file in bytes. The
// kernel prints the value in kB; a bare number is read as bytes.
func readMemTotal(path string) (int64, error) {
	f, err := os.Open(path) // #nosec G304 -- the fixed /proc/meminfo, or a test fixture
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "MemTotal:"))
		if len(fields) == 0 || len(fields) > 2 {
			return 0, fmt.Errorf("%s: malformed line %q", path, line)
		}
		v, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil || v <= 0 {
			return 0, fmt.Errorf("%s: malformed line %q", path, line)
		}
		if len(fields) == 1 {
			return v, nil
		}
		if strings.EqualFold(fields[1], "kB") {
			return v * 1024, nil
		}
		return 0, fmt.Errorf("%s: unknown unit in %q", path, line)
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("%s: %w", path, err)
	}
	return 0, fmt.Errorf("%s: no MemTotal line", path)
}
