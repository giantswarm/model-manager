package lemonade

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

// testMemTotalBytes is 86.07 GB as Lemonade prints it: binary gigabytes.
var testMemTotalBytes = int64(math.Round(86.07 * (1 << 30)))

func listHostNode(t *testing.T, b *Backend) backend.NodeInfo {
	t.Helper()
	nodes, err := b.ListNodes(context.Background())
	require.NoError(t, err)
	require.Len(t, nodes, 1, "the proxied host is the one node")
	return nodes[0]
}

func TestListNodesHostWithoutLoadedModels(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1) // downloaded, not loaded
	f.add("gemma3-4b-FLM", 4.5)
	fixed := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	b.now = func() time.Time { return fixed }
	n := listHostNode(t, b)
	assert.Equal(t, "172.21.0.1", n.Name, "the agent host, as agents dial it, without scheme and port")
	assert.True(t, n.Ready)
	assert.True(t, n.Eligible, "the proxied host is the only serving target")
	assert.Empty(t, n.EligibilityReason)
	assert.Equal(t, "amd64", n.Architecture, "x86_64 normalized to the Kubernetes name")
	assert.Equal(t, testMemTotalBytes, n.AllocatableMemoryBytes)
	assert.Equal(t, testMemTotalBytes, n.BudgetBytes)
	assert.Equal(t, BudgetSourceSystemInfo, n.BudgetSource)
	assert.Equal(t, int64(2), n.GPUCount, "the NPU and the integrated GPU Lemonade found available; the absent NVIDIA GPU does not count")
	assert.Equal(t, "AMD NPU (NPU Strix)", n.GPUProduct, "the NPU names the node's accelerator")
	assert.Nil(t, n.Accelerated, "accelerators are counted, not inferred from loaded models")
	assert.Zero(t, n.GPUMemoryBytes)
	assert.Zero(t, n.ReservedBytes)
	assert.Equal(t, testMemTotalBytes, n.FreeBytes)
	assert.Equal(t, hostNodeMessage, n.Message)
	require.NotNil(t, n.Cache, "Lemonade's model store is the node's cache")
	assert.Equal(t, "/home/lab/.cache/huggingface/hub", n.Cache.MountPath)
	assert.Empty(t, n.Cache.Claim, "no PersistentVolumeClaim behind a host directory")
	assert.Equal(t, 2, n.Cache.Models)
	assert.Equal(t, int64(7_600_000_000), n.Cache.BytesUsed, "the downloaded models' catalog sizes, not the drive's usage")
	assert.Equal(t, fixed, n.Cache.ScannedAt)
	assert.Equal(t, InventoryModels, n.Cache.Inventory)
	assert.False(t, n.Cache.Shared)
}

func TestListNodesReservesLoadedModels(t *testing.T) {
	f, b := newTestBackend(t)
	f.add("qwen3-it-4b-FLM", 3.1)
	f.add("gemma3-4b-FLM", 4.5)
	require.NoError(t, b.Load(context.Background(), backend.LoadRequest{Name: "qwen3-it-4b-FLM"}))
	n := listHostNode(t, b)
	assert.Equal(t, int64(3_100_000_000), n.ReservedBytes, "the loaded model's catalog size")
	assert.Equal(t, testMemTotalBytes-3_100_000_000, n.FreeBytes)
}

func TestListNodesWithoutMemoryFigure(t *testing.T) {
	f, b := newTestBackend(t)
	f.systemInfo = `{"Physical Memory": "unknown", "devices": {"cpu": {"family": "aarch64"}, "amd_gpu": [], "nvidia_gpu": [{"available": true, "name": "NVIDIA RTX 6000", "vram_gb": 48}]}}`
	n := listHostNode(t, b)
	assert.Zero(t, n.BudgetBytes)
	assert.Zero(t, n.AllocatableMemoryBytes)
	assert.Contains(t, n.Message, "host memory unknown")
	assert.Equal(t, "arm64", n.Architecture)
	assert.Equal(t, int64(1), n.GPUCount)
	assert.Equal(t, "NVIDIA RTX 6000", n.GPUProduct, "without an NPU the first GPU names the accelerator")
	assert.Nil(t, n.Cache, "no model store reported, no cache block")
}

func TestListNodesBackendDown(t *testing.T) {
	b, err := New(backend.LemonadeOptions{Endpoint: "http://127.0.0.1:1", Timeout: time.Second})
	require.NoError(t, err)
	_, err = b.ListNodes(context.Background())
	require.Error(t, err)
}

func TestParseMemory(t *testing.T) {
	cases := map[string]int64{
		"86.07 GB": testMemTotalBytes,
		"32.0 GB":  32 << 30,
		"16 GiB":   16 << 30,
		"512 MB":   512 << 20,
		"1.5 TB":   int64(1.5 * (1 << 40)),
		"64":       64 << 30,
	}
	for in, want := range cases {
		got, err := parseMemory(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "unknown", "0 GB", "-1 GB", "12 parsecs", "1 2 3"} {
		_, err := parseMemory(in)
		require.Error(t, err, in)
	}
}

func TestHostOf(t *testing.T) {
	cases := map[string]string{
		"http://172.21.0.1:13305":        "172.21.0.1",
		"http://172.21.0.1:13305/api/v1": "172.21.0.1",
		"https://lemonade.lan":           "lemonade.lan",
		"host.docker.internal:13305":     "host.docker.internal",
		"":                               "",
	}
	for in, want := range cases {
		assert.Equal(t, want, hostOf(in), in)
	}
}
