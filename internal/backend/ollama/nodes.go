package ollama

import (
	"bufio"
	"context"
	"fmt"
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

	// hostNodeMessage explains the host node's numbers to API clients.
	hostNodeMessage = "host memory as seen from the model-manager pod; per-model accelerator share in running.vramBytes"
)

// ListNodes implements backend.NodeLister: the proxied host is the one node.
// Ollama's API has no capacity endpoint (the totals appear only in its startup
// log), so the budget is MemTotal of /proc/meminfo as the pod sees it — on the
// unified-memory laptops Ollama typically runs on, GPU memory is system memory
// anyway — and what loaded models reserve is the sum of `size` over /api/ps.
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
	total, err := readMemTotal(b.meminfoPath)
	if err != nil {
		node.Message = fmt.Sprintf("host memory unknown (%v); %s", err, hostNodeMessage)
	} else {
		node.AllocatableMemoryBytes = total
		node.BudgetBytes = total
	}
	node.FreeBytes = node.BudgetBytes - node.ReservedBytes
	if node.FreeBytes < 0 {
		node.FreeBytes = 0
	}
	return []backend.NodeInfo{node}, nil
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
