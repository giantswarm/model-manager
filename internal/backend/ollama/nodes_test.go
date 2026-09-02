package ollama

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// testMeminfo is the head of /proc/meminfo on an 86 GiB unified-memory laptop.
const testMeminfo = `MemTotal:       90251888 kB
MemFree:         9828720 kB
MemAvailable:   38144696 kB
Buffers:          123456 kB
Cached:         40000000 kB
`

const testMemTotalBytes = int64(90251888 * 1024)

func writeMeminfo(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "meminfo")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func listHostNode(t *testing.T, b *Backend) backend.NodeInfo {
	t.Helper()
	nodes, err := b.ListNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1, "the proxied host is the one node")
	return nodes[0]
}

func TestListNodesHostWithoutLoadedModels(t *testing.T) {
	f, b := newTestBackend(t)
	b.meminfoPath = writeMeminfo(t, testMeminfo)
	f.add("qwen3:0.6b", 522_653_767) // downloaded, not loaded
	n := listHostNode(t, b)
	assert.Equal(t, "172.21.0.1", n.Name, "the agent host, as agents dial it, without scheme and port")
	assert.True(t, n.Ready)
	assert.Equal(t, runtime.GOARCH, n.Architecture)
	assert.Equal(t, testMemTotalBytes, n.AllocatableMemoryBytes)
	assert.Equal(t, testMemTotalBytes, n.BudgetBytes)
	assert.Equal(t, BudgetSourceHostMeminfo, n.BudgetSource)
	assert.Zero(t, n.GPUCount, "accelerators are not observable through Ollama's API")
	assert.Zero(t, n.GPUMemoryBytes)
	assert.Empty(t, n.GPUProduct)
	assert.Zero(t, n.ReservedBytes)
	assert.Equal(t, testMemTotalBytes, n.FreeBytes)
	require.NotNil(t, n.Accelerated)
	assert.False(t, *n.Accelerated, "nothing loaded, nothing on an accelerator")
	assert.Nil(t, n.Cache, "Ollama's store is not a node cache")
	assert.Equal(t, hostNodeMessage, n.Message)
}

func TestListNodesHostWithLoadedModels(t *testing.T) {
	f, b := newTestBackend(t)
	b.meminfoPath = writeMeminfo(t, testMeminfo)
	f.add("qwen3:0.6b", 5_400_000_000)
	f.add("qwen2.5:0.5b", 1_200_000_000)
	f.add("smollm2:135m", 300_000_000)
	ctx := context.Background()
	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen3:0.6b"}))
	require.NoError(t, b.Load(ctx, backend.LoadRequest{Name: "qwen2.5:0.5b"}))

	n := listHostNode(t, b)
	assert.Equal(t, int64(6_600_000_000), n.ReservedBytes, "sum of size over /api/ps, not of the downloads")
	assert.Equal(t, testMemTotalBytes-6_600_000_000, n.FreeBytes)
	assert.Equal(t, testMemTotalBytes, n.BudgetBytes, "the budget does not move with the load")
	require.NotNil(t, n.Accelerated)
	assert.True(t, *n.Accelerated, "size_vram > 0 on a loaded model")

	loaded, err := b.ListLoaded(ctx)
	require.NoError(t, err)
	var sum int64
	for _, lm := range loaded {
		sum += lm.SizeBytes
		assert.Positive(t, lm.VRAMBytes, "running.vramBytes keeps the per-model accelerator share")
	}
	assert.Equal(t, sum, n.ReservedBytes, "reserved matches what /loaded reports")

	require.NoError(t, b.Unload(ctx, "qwen3:0.6b"))
	n = listHostNode(t, b)
	assert.Equal(t, int64(1_200_000_000), n.ReservedBytes)
	assert.Equal(t, testMemTotalBytes-1_200_000_000, n.FreeBytes)
}

func TestListNodesCPUOnlyHost(t *testing.T) {
	f, b := newTestBackend(t)
	b.meminfoPath = writeMeminfo(t, testMeminfo)
	f.cpuOnly = true
	f.add("qwen3:0.6b", 5_400_000_000)
	require.NoError(t, b.Load(context.Background(), backend.LoadRequest{Name: "qwen3:0.6b"}))
	n := listHostNode(t, b)
	assert.Equal(t, int64(5_400_000_000), n.ReservedBytes)
	require.NotNil(t, n.Accelerated)
	assert.False(t, *n.Accelerated, "loaded, but size_vram 0 everywhere")
}

func TestListNodesMeminfoUnreadable(t *testing.T) {
	f, b := newTestBackend(t)
	b.meminfoPath = filepath.Join(t.TempDir(), "missing")
	f.add("qwen3:0.6b", 5_400_000_000)
	require.NoError(t, b.Load(context.Background(), backend.LoadRequest{Name: "qwen3:0.6b"}))
	n := listHostNode(t, b)
	assert.Zero(t, n.AllocatableMemoryBytes)
	assert.Zero(t, n.BudgetBytes)
	assert.Equal(t, BudgetSourceHostMeminfo, n.BudgetSource, "the source stays so clients recognise the host row")
	assert.Equal(t, int64(5_400_000_000), n.ReservedBytes, "reservations do not depend on the budget")
	assert.Zero(t, n.FreeBytes, "clamped, never negative")
	assert.Contains(t, n.Message, "host memory unknown")
	assert.Contains(t, n.Message, "missing")
	assert.Contains(t, n.Message, hostNodeMessage)
}

func TestListNodesBackendDown(t *testing.T) {
	b, err := New(backend.OllamaOptions{Endpoint: "http://127.0.0.1:1", Timeout: time.Second})
	require.NoError(t, err)
	_, err = b.ListNodes(context.Background())
	require.Error(t, err, "no /api/ps, no node: the API answers backend_error, not a node with made-up numbers")
}

func TestListNodesNameFollowsAgentHost(t *testing.T) {
	f := newFakeOllama(t)
	b, err := New(backend.OllamaOptions{Endpoint: f.srv.URL, AgentHost: "http://ollama.lab.example:11434"})
	require.NoError(t, err)
	b.meminfoPath = writeMeminfo(t, testMeminfo)
	assert.Equal(t, "ollama.lab.example", listHostNode(t, b).Name)

	b, err = New(backend.OllamaOptions{Endpoint: f.srv.URL})
	require.NoError(t, err)
	b.meminfoPath = writeMeminfo(t, testMeminfo)
	assert.Equal(t, "127.0.0.1", listHostNode(t, b).Name, "defaults to the endpoint's host")
}

func TestHostOf(t *testing.T) {
	for in, want := range map[string]string{
		"http://172.21.0.1:11434":     "172.21.0.1",
		"https://ollama.example/":     "ollama.example",
		"172.21.0.1:11434":            "172.21.0.1",
		"host.docker.internal:11434":  "host.docker.internal",
		"http://[fd00::1]:11434":      "fd00::1",
		" http://ollama.example:80/ ": "ollama.example",
		"":                            "",
	} {
		assert.Equal(t, want, hostOf(in), in)
	}
}

func TestReadMemTotal(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    int64
		wantErr string
	}{
		{name: "kernel format", content: testMeminfo, want: testMemTotalBytes},
		{name: "bytes without unit", content: "MemTotal: 1024\n", want: 1024},
		{name: "lower-case unit", content: "MemTotal: 2 kb\n", want: 2048},
		{name: "no MemTotal line", content: "MemFree: 1 kB\n", wantErr: "no MemTotal line"},
		{name: "malformed value", content: "MemTotal: lots kB\n", wantErr: "malformed"},
		{name: "zero", content: "MemTotal: 0 kB\n", wantErr: "malformed"},
		{name: "unknown unit", content: "MemTotal: 1 MB\n", wantErr: "unknown unit"},
		{name: "empty", content: "", wantErr: "no MemTotal line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readMemTotal(writeMeminfo(t, tc.content))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
	_, err := readMemTotal(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
}
