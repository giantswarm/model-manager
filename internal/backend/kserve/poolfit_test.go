package kserve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The g6 family's L4 sizes as cluster-manager lists them: nominal shape, one
// 24 GiB GPU on the small sizes, and what a node leaves a predictor after the
// kubelet's reservations and the fleet's daemonsets (usable = vcpu − 1,
// memory × 0.95 − 3.3, the node model measured on gazelle).
var (
	shapeXLarge  = backend.InstanceShape{InstanceType: "g6.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}
	shape2XLarge = backend.InstanceShape{InstanceType: "g6.2xlarge", Size: "2xlarge", VCPU: 8, MemoryGiB: 32, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 7, UsableMemoryGiB: 27.1}
	// shapeL40S is a g6e.xlarge: the same nominal shape as the g6.xlarge
	// but 32 GiB of memory and a 48 GiB GPU.
	shapeL40S = backend.InstanceShape{InstanceType: "g6e.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 32, GPUs: 1, GPUMemoryGiB: 48, UsableVCPU: 3, UsableMemoryGiB: 27.1}
	// shapeL40S2XLarge and shape12XLarge are the families' larger sizes: one
	// 48 GiB L40S on a g6e.2xlarge, four 24 GiB L4s on a g6.12xlarge.
	shapeL40S2XLarge = backend.InstanceShape{InstanceType: "g6e.2xlarge", Size: "2xlarge", VCPU: 8, MemoryGiB: 64, GPUs: 1, GPUMemoryGiB: 48, UsableVCPU: 7, UsableMemoryGiB: 57.5}
	shape12XLarge    = backend.InstanceShape{InstanceType: "g6.12xlarge", Size: "12xlarge", VCPU: 48, MemoryGiB: 192, GPUs: 4, GPUMemoryGiB: 24, UsableVCPU: 47, UsableMemoryGiB: 179.1}
)

const fatRepo = "org/fat"

// fatPresetDoc is a preset requesting what the gazelle predictor of
// giantswarm/agent-platform#502 did — 4 vCPU / 16 GiB — for a model the hub
// does not know, so the preset's requirements size it.
func fatPresetDoc() string {
	return strings.Replace(presetDoc("fat", fatRepo, 8, ""), `requests: {cpu: "2", memory: 8Gi}`, `requests: {cpu: 4, memory: 16Gi}`, 1)
}

// setPool configures the GPU pool — taint, selector and the given shapes —
// as a registered document does (the option), over a discovery ConfigMap
// naming the same pool without shapes.
func (f *fixture) setPool(ctx context.Context, shapes ...backend.InstanceShape) {
	f.t.Helper()
	pool := *poolInput()
	pool.Instances = shapes
	f.b.opts.GPUPool = pool
	f.b.cfg.opts.GPUPool = pool
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
}

func servingObjects(t *testing.T, f *fixture, ctx context.Context) int {
	t.Helper()
	list, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	return len(list.Items)
}

// TestFitCheckPoolShapesNameTheSize: the pool has no node and the document
// lists its shapes. The answer is judged against them — the smallest size
// hosting the predictor is the node it will come as — and is no longer
// unverified; Load creates the serving object.
func TestFitCheckPoolShapesNameTheSize(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.setPool(ctx, shape2XLarge, shapeXLarge)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Equal(t, "g6.xlarge", res.InstanceType, "the smallest size that hosts it, whatever the document's order")
	assert.Equal(t, int64(24)*gib, res.BudgetBytes, "the size's GPU memory is the budget")
	assert.Equal(t, res.BudgetBytes, res.FreeBytes, "a node that does not exist yet has nothing reserved")
	assert.Contains(t, res.Reason, "no node in the GPU pool yet ("+poolLabel+"="+poolName+"): the pool scales from zero")
	assert.Contains(t, res.Reason, "the node comes as xlarge (g6.xlarge: 3 vCPU / 11.9 GiB for the predictor, 1 × 24 GiB GPU)")
	assert.Contains(t, res.Reason, "xlarge hosts 2 vCPU / 8 GiB requested, 1 GPU, 1 GiB of GPU memory")
	assert.NotContains(t, res.Reason, "unverified")

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	assert.Equal(t, 1, servingObjects(t, f, ctx), "the serving object exists; its pending predictor brings the node")

	// The explicit node keeps its refusal: the shapes judge the pool only.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Contains(t, res.Reason, "outside the GPU pool node selector")
}

// TestFitCheckPoolShapesRefuseWhatNoSizeHosts: a preset requesting 4 vCPU /
// 16 GiB on a pool of xlarge nodes, which leave a predictor 3 vCPU / 11.9 GiB.
// The answer is no and says what the pool's largest size leaves; Load is
// refused before any object is created. Widened by a 2xlarge, the pool hosts
// it and the answer names that size.
func TestFitCheckPoolShapesRefuseWhatNoSizeHosts(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("fat", fatPresetDoc()))
	f.setPool(ctx, shapeXLarge)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "fat"})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Empty(t, res.InstanceType)
	assert.Equal(t, weightsSourcePreset, res.WeightsSource, "the hub does not know the model; the preset sizes it")
	assert.Contains(t, res.Reason, "no size of the pool (xlarge) hosts preset fat: 4 vCPU / 16 GiB requested, 1 GPU, 9 GiB of GPU memory")
	assert.Contains(t, res.Reason, "xlarge, the largest, leaves a predictor 3 vCPU / 11.9 GiB after the node's kubelet reservations and daemonsets and carries 1 × 24 GiB GPU")
	assert.Contains(t, res.Reason, "widen the pool's sizes or pick a smaller preset")

	err = f.b.Load(ctx, backend.LoadRequest{Preset: "fat"})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "no size of the pool (xlarge) hosts preset fat")
	assert.Equal(t, 0, servingObjects(t, f, ctx), "nothing is created that could only sit Pending")
	err = f.b.Pull(ctx, backend.PullRequest{Ref: fatRepo, Preset: "fat"}, nil)
	require.ErrorIs(t, err, backend.ErrUnfit, "a pull onto a pool that can never serve the model is refused as well")

	f.setPool(ctx, shapeXLarge, shape2XLarge)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "fat"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g6.2xlarge", res.InstanceType, "the smallest size that hosts it")
	assert.Contains(t, res.Reason, "the node comes as 2xlarge")
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "fat"}))
	assert.Equal(t, 1, servingObjects(t, f, ctx))
}

// TestFitCheckPoolShapesJudgeGPUMemory: the GPU side — a preset whose
// weights and overhead exceed the size's GPU memory is refused although its
// CPU and memory requests fit, and a model without a preset is judged on
// its weights and the default overhead against the GPU memory alone.
func TestFitCheckPoolShapesJudgeGPUMemory(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.setPool(ctx, shapeXLarge, shape2XLarge)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: bigRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, int64(100)*gib, res.WeightsBytes, "the hub's safetensors index sizes the weights")
	assert.Contains(t, res.Reason, "no size of the pool (xlarge, 2xlarge) hosts preset big: 2 vCPU / 8 GiB requested, 1 GPU, 101 GiB of GPU memory")
	assert.Contains(t, res.Reason, "carries 1 × 24 GiB GPU, 24.0 GiB on the 1 GPU the predictor requests (100.0 GiB weights (safetensors-index) + 1.0 GiB overhead = 101.0 GiB needed)")

	// No preset: 10 GiB of weights and the 30 GiB default overhead need 40
	// GiB of GPU memory; an L4 has 24, an L40S 48.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: presetlessRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Empty(t, res.Preset)
	assert.Zero(t, res.DeclaredWeightsBytes, "no preset, no declaration")
	assert.Contains(t, res.Reason, "hosts "+presetlessRepo+": 40 GiB of GPU memory on 1 GPU (no preset: weights and default overhead only)")

	f.setPool(ctx, shapeL40S)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: presetlessRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g6e.xlarge", res.InstanceType)
	assert.Equal(t, int64(48)*gib, res.BudgetBytes)
}

// TestFitCheckPoolShapesFromDiscovery: the discovery ConfigMap's spec.gpuPool
// may carry the shapes too; the document's replace them; and a bad shape
// there is dropped, so the answer is the unverified one instead of a wrong
// verdict.
func TestFitCheckPoolShapesFromDiscovery(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("fat", fatPresetDoc()))
	viaDiscovery := poolInput()
	viaDiscovery.Instances = []backend.InstanceShape{shapeXLarge}
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: viaDiscovery})

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "fat"})
	require.NoError(t, err)
	assert.False(t, res.Fits, "discovery's xlarge cannot host the fat preset")
	assert.Contains(t, res.Reason, "no size of the pool (xlarge)")

	// The document's shapes replace discovery's.
	f.b.opts.GPUPool = backend.GPUPool{Instances: []backend.InstanceShape{shape2XLarge}}
	f.b.cfg.opts.GPUPool = f.b.opts.GPUPool
	f.resetSettings()
	s := f.b.cfg.settings(ctx)
	assert.Equal(t, []backend.InstanceShape{shape2XLarge}, s.GPUPool.Instances)
	assert.Equal(t, map[string]string{poolLabel: poolName}, s.GPUPool.NodeSelector, "discovery's selector stays")
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "fat"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g6.2xlarge", res.InstanceType)

	// A bad shape in discovery (a chart value nothing validated) is ignored.
	f.b.opts.GPUPool = backend.GPUPool{}
	f.b.cfg.opts.GPUPool = f.b.opts.GPUPool
	bad := poolInput()
	bad.Instances = []backend.InstanceShape{{InstanceType: "g6.xlarge", VCPU: 0, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9}}
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: bad})
	assert.Empty(t, f.b.cfg.settings(ctx).GPUPool.Instances)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "fat"})
	require.NoError(t, err)
	assert.True(t, res.Fits)
	assert.Empty(t, res.InstanceType)
	assert.Contains(t, res.Reason, "unverified", "without usable shapes the answer is the one before the shapes existed")
}

// TestFitCheckPoolShapesBudgetTheRequestedGPUs: the GPU memory a predictor
// has is that of the GPUs it requests, not the node's. A one-GPU preset
// needing 30 GiB is refused on a pool whose only size carries 4 × 24 GiB —
// scheduled with one GPU, vLLM would have one card — while a two-GPU preset
// needing 40 GiB fits that size with a 48 GiB budget; the refusal and the
// fit both name the budget of the requested GPUs. With a single-GPU 48 GiB
// size beside it, the one-GPU preset comes as that size.
func TestFitCheckPoolShapesBudgetTheRequestedGPUs(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t,
		presetConfigMap("one", presetDoc("one", fatRepo, 29, "")),
		presetConfigMap("two", strings.Replace(presetDoc("two", fatRepo, 39, ""), "gpus: 1", "gpus: 2", 1)),
	)
	f.setPool(ctx, shape12XLarge)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "one"})
	require.NoError(t, err)
	assert.False(t, res.Fits, "30 GiB on one 24 GiB GPU, whatever the node's four add up to")
	assert.Equal(t, int64(24)*gib, res.BudgetBytes, "the budget is one GPU's memory")
	assert.Contains(t, res.Reason, "no size of the pool (12xlarge) hosts preset one: 2 vCPU / 8 GiB requested, 1 GPU, 30 GiB of GPU memory")
	assert.Contains(t, res.Reason, "carries 4 × 24 GiB GPU, 24.0 GiB on the 1 GPU the predictor requests (29.0 GiB weights (preset) + 1.0 GiB overhead = 30.0 GiB needed)")
	assert.NotContains(t, res.Reason, "declares", "the preset sized the weights itself: nothing to reconcile")
	err = f.b.Load(ctx, backend.LoadRequest{Preset: "one"})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Equal(t, 0, servingObjects(t, f, ctx))

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "two"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g6.12xlarge", res.InstanceType)
	assert.Equal(t, int64(48)*gib, res.BudgetBytes, "two of the four GPUs")
	assert.Equal(t, res.BudgetBytes, res.FreeBytes)
	assert.Contains(t, res.Reason, "39.0 GiB weights (preset) + 1.0 GiB overhead = 40.0 GiB fit within 48.0 GiB on the 2 GPU the predictor requests")
	assert.Contains(t, res.Reason, "12xlarge hosts 2 vCPU / 8 GiB requested, 2 GPU, 40 GiB of GPU memory")

	f.setPool(ctx, shape12XLarge, shapeL40S2XLarge)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "one"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, "g6e.2xlarge", res.InstanceType, "the one-GPU preset comes as the size with the 48 GiB card")
	assert.Equal(t, int64(48)*gib, res.BudgetBytes)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "two"})
	require.NoError(t, err)
	assert.Equal(t, "g6.12xlarge", res.InstanceType, "two GPUs: the 2xlarge carries one")
}

// TestFitCheckPoolShapesNameTheDeclaration: the pool was sized on the form
// from the preset's declared weights; the fit sizes them from the hub. When
// the hub holds more than the preset declares, both verdicts say so beside
// declaredWeightsBytes and load_model's refusal carries the same words; a
// declaration that covers the hub adds nothing.
func TestFitCheckPoolShapesNameTheDeclaration(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t,
		// The hub's tree holds 10 GiB of the repository; the preset declares 4.
		presetConfigMap("lean", presetDoc("lean", presetlessRepo, 4, "")),
		// The hub's safetensors index holds 100 GiB; the preset declares 15.
		presetConfigMap("wishful", presetDoc("wishful", bigRepo, 15, "")),
	)
	f.setPool(ctx, shapeXLarge)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "lean"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, weightsSourceTree, res.WeightsSource)
	assert.Equal(t, int64(10)*gib, res.WeightsBytes)
	assert.Equal(t, int64(4)*gib, res.DeclaredWeightsBytes)
	assert.Contains(t, res.Reason, "xlarge hosts 2 vCPU / 8 GiB requested, 1 GPU, 11 GiB of GPU memory; the preset declares 4.0 GiB of weights, the Hub holds 10.0 GiB")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "wishful"})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, weightsSourceIndex, res.WeightsSource)
	assert.Equal(t, int64(15)*gib, res.DeclaredWeightsBytes)
	assert.Contains(t, res.Reason, "no size of the pool (xlarge) hosts preset wishful: 2 vCPU / 8 GiB requested, 1 GPU, 101 GiB of GPU memory")
	assert.Contains(t, res.Reason, "widen the pool's sizes or pick a smaller preset; the preset declares 15.0 GiB of weights; the Hub holds 100.0 GiB, which is what does not fit — correct the preset")
	err = f.b.Load(ctx, backend.LoadRequest{Preset: "wishful"})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "the Hub holds 100.0 GiB, which is what does not fit — correct the preset", "load_model echoes the fit")
	assert.Equal(t, 0, servingObjects(t, f, ctx))

	// The shipped preset for the repository declares what the hub holds.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "big"})
	require.NoError(t, err)
	assert.Equal(t, res.WeightsBytes, res.DeclaredWeightsBytes)
	assert.NotContains(t, res.Reason, "declares")
}

// An unparsable request is the preset's fault and is named as such.
func TestFitCheckPoolShapesNameABadRequest(t *testing.T) {
	ctx := context.Background()
	doc := strings.Replace(presetDoc("odd", fatRepo, 8, ""), `requests: {cpu: "2", memory: 8Gi}`, `requests: {cpu: "two", memory: 8Gi}`, 1)
	f := newFixture(t, presetConfigMap("odd", doc))
	f.setPool(ctx, shapeXLarge)
	_, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "odd"})
	require.ErrorIs(t, err, backend.ErrInvalid)
	assert.Contains(t, err.Error(), `preset odd: resources.requests.cpu "two"`)
}

// TestFitCheckTakesThePoolPathOnceTheDiscoveryDocumentAppears
// (giantswarm/model-manager#127): no node advertises a GPU, and the discovery
// ConfigMap — with the pool — is published a moment after the settings were
// cached without it, the way the serving slice publishes it while
// installing. The first fit says the document is not published and to retry;
// the next, a second later and well within DiscoveryTTL, takes the pool
// path. "no accelerator node" is the verdict for a document that names no
// pool.
func TestFitCheckTakesThePoolPathOnceTheDiscoveryDocumentAppears(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	now := time.Now()
	f.b.cfg.now = func() time.Time { return now }
	for _, n := range []string{testCacheNode, testGPUNode, testCPUNode} {
		require.NoError(t, f.cs.CoreV1().Nodes().Delete(ctx, n, metav1.DeleteOptions{}))
	}
	require.NoError(t, f.cs.CoreV1().ConfigMaps(testPlatformNS).Delete(ctx, DefaultDiscoveryConfigMap, metav1.DeleteOptions{}))
	f.resetSettings()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.True(t, res.Retryable)
	assert.Equal(t, "the serving layer's discovery document "+testPlatformNS+"/"+DefaultDiscoveryConfigMap+" is not published yet — the slice is still installing; retry in a moment", res.Reason)
	assert.Empty(t, res.BudgetSource)

	_, err = f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, discoveryConfigMapWith(discoveryOpts{gpuPool: poolInput()}), metav1.CreateOptions{})
	require.NoError(t, err)
	now = now.Add(time.Second)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.False(t, res.Retryable)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Contains(t, res.Reason, "no node in the GPU pool yet ("+poolLabel+"="+poolName+"): the pool scales from zero")

	// A document that names no pool: nothing scales from zero, and the
	// nodes are the verdict.
	f.setDiscoveryOpts(ctx, discoveryOpts{})
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.False(t, res.Retryable)
	assert.Contains(t, res.Reason, "no accelerator node: no node advertises "+DefaultGPUResourceName)
}
