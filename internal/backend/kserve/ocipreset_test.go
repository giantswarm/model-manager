package kserve

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// ociImage is the storage URI of the modelcar preset: the weights are an OCI
// image containerd pulls onto the node, not a download into the cache claim.
const ociImage = "oci://registry.example/models/tiny-clone:abc123"

// ociPresetDoc is a preset for presetlessRepo whose storageUri is an OCI
// model image. The hub still knows the repository (a 10 GiB tree), so the
// fit sizes the weights from there like for any preset.
func ociPresetDoc() string {
	return strings.Replace(presetDoc("modelcar", presetlessRepo, 10, ""), "storageUri: hf://"+presetlessRepo, "storageUri: "+ociImage, 1)
}

// hubCalls is how many requests the fake hub has answered.
func (h *fakeHub) hubCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// TestPresetStoresInCache: the storage scheme decides — a bare model
// reference and every download scheme store in the cache; an OCI model
// image does not.
func TestPresetStoresInCache(t *testing.T) {
	var bare *servingPreset
	assert.True(t, bare.storesInCache(), "a bare model reference is a Hugging Face download into the cache")
	for _, uri := range []string{"hf://" + tinyRepo, "pvc://hf-cache/tiny", "s3://bucket/tiny"} {
		p, err := parsePreset([]byte(strings.Replace(presetDoc("tiny", tinyRepo, 1, ""), "storageUri: hf://"+tinyRepo, "storageUri: "+uri, 1)), "shipped")
		require.NoError(t, err)
		assert.True(t, p.storesInCache(), uri)
	}
	implicit, err := parsePreset([]byte(strings.Replace(presetDoc("tiny", tinyRepo, 1, ""), "    storageUri: hf://"+tinyRepo+"\n", "", 1)), "shipped")
	require.NoError(t, err)
	assert.Equal(t, "hf://"+tinyRepo, implicit.Spec.Model.StorageURI)
	assert.True(t, implicit.storesInCache(), "no storageUri means the hub")
	oci, err := parsePreset([]byte(ociPresetDoc()), "shipped")
	require.NoError(t, err)
	assert.False(t, oci.storesInCache())
}

// TestOCIPresetPlacesOnAnyEligibleNode: the cache claim is pinned to n1 and
// the redirect policy mounts it into every predictor, so gpu1 is no serving
// target for an hf:// preset. An oci:// preset never touches the claim: it is
// placed on gpu1 when the request names it, picked over the cache node when
// it does not (the larger free budget wins; no cache-node preference), judged
// not cached from the oci-image source, and load_model creates its serving
// object there. The hf:// preset keeps the pin's refusal and the cache node;
// list_nodes, which describes the nodes and not a preset, still reports the
// pin.
func TestOCIPresetPlacesOnAnyEligibleNode(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("modelcar", ociPresetDoc()))
	f.setDiscovery(ctx, nil, true)
	const pinReason = "cache claim hf-cache is pinned to n1"

	// Named: the oci:// preset is placed on the node the claim excludes.
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar", Node: testGPUNode})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testGPUNode, res.Node)
	assert.Equal(t, int64(10)*gib, res.WeightsBytes, "the hub sizes the model as for any preset")
	assert.Equal(t, weightsSourceTree, res.WeightsSource)
	assert.False(t, res.Cached)
	assert.Equal(t, backend.CacheSourceOCIImage, res.CacheSource)

	// Unnamed: the eligible node with the most free budget — gpu1's 128 GiB
	// over the cache node's 64 GiB; the hf:// preset prefers the cache node.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testGPUNode, res.Node, "no cache-node preference for an oci:// preset")
	assert.Equal(t, backend.CacheSourceOCIImage, res.CacheSource)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.Equal(t, testCacheNode, res.Node, "an hf:// preset keeps the cache node")
	assert.NotEqual(t, backend.CacheSourceOCIImage, res.CacheSource, "the claim answers for an hf:// preset")

	// The hf:// preset keeps the pin's refusal on gpu1, on the fit check and on
	// load_model, before any object exists.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, "node gpu1 is not a serving target: "+pinReason, res.Reason)
	err = f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo, Node: testGPUNode})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.ErrorContains(t, err, pinReason)
	assert.Equal(t, 0, servingObjects(t, f, ctx))

	// list_nodes is unchanged: the pin is a fact about the node.
	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		if n.Name == testGPUNode {
			assert.False(t, n.Eligible)
			assert.Equal(t, pinReason, n.EligibilityReason)
		}
	}

	// load_model places the oci:// preset on gpu1; the object carries the image.
	got, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "modelcar", Node: testGPUNode})
	require.NoError(t, err)
	require.NotNil(t, got.Fit)
	assert.Equal(t, testGPUNode, got.Fit.Node)
	assert.Equal(t, backend.CacheSourceOCIImage, got.Fit.CacheSource)
	assert.Equal(t, 1, servingObjects(t, f, ctx))
	uri, _, err := unstructured.NestedString(f.isvc(ctx, "modelcar"), "spec", "predictor", "model", "storageUri")
	require.NoError(t, err)
	assert.Equal(t, ociImage, uri)
}

// TestOCIPresetOnPoolAtZero: on a GPU pool without a node the oci:// preset
// is judged against the pool's shapes like any other, and the cache verdict
// is the oci-image one instead of asking the claim.
func TestOCIPresetOnPoolAtZero(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("modelcar", ociPresetDoc()))
	f.setPool(ctx, shapeL40S)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Equal(t, "g6e.xlarge", res.InstanceType)
	assert.False(t, res.Cached)
	assert.Equal(t, backend.CacheSourceOCIImage, res.CacheSource)
}

// TestPullRefusesAnOCIPreset: pull_model on an oci:// preset — named, resolved
// from its model, or given as the reference — is refused as invalid before
// the hub is asked, naming the image the platform pre-pulls, and no Job
// exists. The hf:// preset's pull keeps its verdicts: refused on the
// pinned-out node with the pin's reason, after the hub sized it.
func TestPullRefusesAnOCIPreset(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("modelcar", ociPresetDoc()))
	f.setDiscovery(ctx, nil, true)

	const refusal = "invalid request: " + presetlessRepo + " is served from an OCI model image the platform pre-pulls on its GPU nodes (preset modelcar, " + ociImage + "); nothing to download"
	for _, req := range []backend.PullRequest{
		{Ref: presetlessRepo, Preset: "modelcar"},
		{Ref: presetlessRepo},
		{Ref: "modelcar"},
		{Ref: presetlessRepo, Preset: "modelcar", Node: testGPUNode},
	} {
		err := f.b.Pull(ctx, req, nil)
		require.ErrorIs(t, err, backend.ErrInvalid, "%+v", req)
		assert.EqualError(t, err, refusal, "%+v", req)
	}
	assert.Zero(t, f.hub.hubCalls(), "refused before any hub call")
	jobList, err := f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobList.Items, "no download Job for an OCI model image")

	err = f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo, Node: testGPUNode}, nil)
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.ErrorContains(t, err, "cache claim hf-cache is pinned to n1")
	assert.Positive(t, f.hub.hubCalls(), "an hf:// preset is sized by the hub as before")
	jobList, err = f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobList.Items)
}
