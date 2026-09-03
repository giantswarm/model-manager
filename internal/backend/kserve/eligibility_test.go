package kserve

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// TestNodesAreAcceleratorsWithEligibility is the spidertron shape: two GPU
// nodes of which one holds the node-local cache and is named by the serving
// node selector, a GPU node that is down, CPU-only nodes, and the Kyverno
// redirect policy mounting the cache claim into every predictor. Only the
// accelerator nodes are reported, each says whether it can serve, and a
// request naming a node that cannot is refused with the reason before any
// Job or InferenceService exists — through the driver and through the REST API.
func TestNodesAreAcceleratorsWithEligibility(t *testing.T) {
	ctx := context.Background()
	const downNode = "gpu-down"
	f := newFixture(t,
		node("cpu2", "62Gi", nil),
		notReady(node(downNode, "128Gi", map[string]string{labelGPUPresent: "true"})),
	)
	f.setDiscovery(ctx, map[string]string{labelHostname: testCacheNode}, true)

	byName := func(nodes []backend.NodeInfo) map[string]backend.NodeInfo {
		out := map[string]backend.NodeInfo{}
		for _, n := range nodes {
			out[n.Name] = n
		}
		return out
	}

	// CPU-only nodes are gone; the three accelerator nodes stay, by name.
	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	assert.Equal(t, []string{downNode, testGPUNode, testCacheNode}, names, "accelerator nodes only, sorted")
	got := byName(nodes)
	cache := got[testCacheNode]
	assert.True(t, cache.Eligible)
	assert.Empty(t, cache.EligibilityReason)
	assert.EqualValues(t, 1, cache.GPUCount, "GPU resource without feature-discovery labels")
	require.NotNil(t, cache.Cache)
	gpu := got[testGPUNode]
	assert.False(t, gpu.Eligible)
	assert.Equal(t, "outside the serving node selector (kubernetes.io/hostname=n1); cache claim hf-cache is pinned to n1", gpu.EligibilityReason)
	assert.Nil(t, gpu.Cache, "no cache on a node the claim cannot follow")
	down := got[downNode]
	assert.False(t, down.Ready)
	assert.False(t, down.Eligible)
	assert.Contains(t, down.EligibilityReason, "not ready")

	// An explicit node that is not a serving target fails the fit check with
	// its reason; a CPU node is not found among the accelerator nodes; the
	// default picks the eligible cache node.
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, "node gpu1 is not a serving target: "+gpu.EligibilityReason, res.Reason)
	assert.Empty(t, res.Node)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: "cpu2"})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Contains(t, res.Reason, `node "cpu2" not found`)
	assert.Contains(t, res.Reason, DefaultGPUResourceName)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testCacheNode, res.Node)

	// Serve and pull refuse the node before creating anything.
	err = f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo, Node: testGPUNode})
	assert.ErrorIs(t, err, backend.ErrUnfit)
	assert.ErrorContains(t, err, "node gpu1 is not a serving target")
	isvcs, err := f.dyn.Resource(isvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, isvcs.Items, "no InferenceService for an ineligible node")
	err = f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo, Node: testGPUNode}, nil)
	assert.ErrorIs(t, err, backend.ErrUnfit)
	assert.ErrorContains(t, err, "cache claim hf-cache is pinned to n1")
	jobList, err := f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobList.Items, "no download Job for an ineligible node")

	// The same through the REST API: the flag and reason on GET /nodes, the
	// reason as 412 does_not_fit on load, fits=false on the fit check.
	svc := service.New(f.b, jobs.NewManager(), nil, nil, service.Config{}, nil)
	mux := http.NewServeMux()
	api.NewREST(svc, nil).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	call := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			require.NoError(t, json.NewEncoder(&buf).Encode(body))
		}
		req, err := http.NewRequestWithContext(ctx, method, srv.URL+path, &buf)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		out := map[string]any{}
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&out))
		return resp.StatusCode, out
	}
	status, body := call(http.MethodGet, api.Prefix+"/nodes", nil)
	require.Equal(t, http.StatusOK, status)
	listed := body["nodes"].([]any)
	require.Len(t, listed, 3)
	for _, raw := range listed {
		n := raw.(map[string]any)
		switch n["name"] {
		case testCacheNode:
			assert.Equal(t, true, n["eligible"])
			_, has := n["eligibilityReason"]
			assert.False(t, has, "no reason on an eligible node")
		case testGPUNode:
			assert.Equal(t, false, n["eligible"])
			assert.Equal(t, gpu.EligibilityReason, n["eligibilityReason"])
		}
	}
	status, body = call(http.MethodPost, api.Prefix+"/models/load", map[string]any{"model": tinyRepo, "node": testGPUNode})
	assert.Equal(t, http.StatusPreconditionFailed, status)
	apiErr := body["error"].(map[string]any)
	assert.Equal(t, "does_not_fit", apiErr["code"])
	assert.Contains(t, apiErr["message"], gpu.EligibilityReason)
	status, body = call(http.MethodPost, api.Prefix+"/models/fit-check", map[string]any{"model": tinyRepo, "node": testGPUNode})
	assert.Equal(t, http.StatusOK, status)
	assert.Equal(t, false, body["fits"])
	assert.Contains(t, body["reason"], gpu.EligibilityReason)

	// Without a serving node selector only the pinned cache disqualifies the
	// GPU node ...
	f.setDiscovery(ctx, nil, true)
	nodes, err = f.b.ListNodes(ctx)
	require.NoError(t, err)
	assert.Equal(t, "cache claim hf-cache is pinned to n1", byName(nodes)[testGPUNode].EligibilityReason)

	// ... and without the redirect policy predictors download the weights
	// themselves, so the cache location no longer decides. A node that is
	// down stays out.
	f.setDiscovery(ctx, nil, false)
	nodes, err = f.b.ListNodes(ctx)
	require.NoError(t, err)
	got = byName(nodes)
	assert.True(t, got[testGPUNode].Eligible, got[testGPUNode].EligibilityReason)
	assert.True(t, got[testCacheNode].Eligible)
	assert.Equal(t, "not ready", got[downNode].EligibilityReason)

	// The default choice never falls back to an ineligible node: with the
	// selector naming the node that is down, nothing qualifies and the
	// answer lists every node's reason.
	f.setDiscovery(ctx, map[string]string{labelHostname: downNode}, true)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Contains(t, res.Reason, "no eligible node: ")
	assert.Contains(t, res.Reason, downNode+": not ready; cache claim hf-cache is pinned to n1")
	assert.Contains(t, res.Reason, testCacheNode+": outside the serving node selector (kubernetes.io/hostname="+downNode+")")
	err = f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo})
	assert.ErrorIs(t, err, backend.ErrUnfit)
	isvcs, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, isvcs.Items)
}
