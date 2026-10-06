package kserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The GPU node pool the platform creates: tainted nvidia.com/gpu NoSchedule,
// labelled giantswarm.io/machine-pool=<cluster>-<pool>.
const (
	poolTaintKey      = "nvidia.com/gpu"
	poolLabel         = "giantswarm.io/machine-pool"
	poolName          = "c1-gpu01"
	poolNode          = "pool1"
	poolToleranceYAML = `  scheduling:
    tolerations:
      - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
      - {key: dedicated, operator: Equal, value: llm, effect: NoExecute}
`
)

func poolInput() *backend.GPUPool {
	return &backend.GPUPool{
		Taint:        &backend.Taint{Key: poolTaintKey, Effect: "NoSchedule"},
		NodeSelector: map[string]string{poolLabel: poolName},
	}
}

// taintedPoolNode is a GPU node of the pool: the taint, the pool label, the
// feature-discovery labels the operator adds.
func taintedPoolNode() *corev1.Node {
	n := node(poolNode, "256Gi", map[string]string{poolLabel: poolName, labelGPUCount: "1", labelGPUMemory: "24576", labelGPUProduct: "NVIDIA-L4"})
	n.Spec.Taints = []corev1.Taint{{Key: poolTaintKey, Effect: corev1.TaintEffectNoSchedule}}
	return n
}

func TestPoolTolerationFromTaint(t *testing.T) {
	_, ok := settings{}.poolToleration()
	assert.False(t, ok, "no taint, no toleration")
	_, ok = settings{GPUPool: backend.GPUPool{Taint: &backend.Taint{}}}.poolToleration()
	assert.False(t, ok, "a taint without a key is no taint")

	tol, ok := settings{GPUPool: *poolInput()}.poolToleration()
	require.True(t, ok)
	assert.Equal(t, corev1.Toleration{Key: poolTaintKey, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule}, tol, "no value: Exists")

	tol, _ = settings{GPUPool: backend.GPUPool{Taint: &backend.Taint{Key: "dedicated", Value: "llm"}}}.poolToleration()
	assert.Equal(t, corev1.Toleration{Key: "dedicated", Operator: corev1.TolerationOpEqual, Value: "llm"}, tol, "a value: Equal; no effect: every effect")
	assert.Equal(t, map[string]any{"key": "dedicated", "operator": "Equal", "value": "llm"}, tolerationMap(tol))
}

func TestScheduleOwnPods(t *testing.T) {
	s := settings{GPUPool: *poolInput()}

	pinned := &corev1.PodSpec{NodeName: "n1"}
	s.schedule(pinned)
	assert.Equal(t, s.poolTolerations(), pinned.Tolerations, "the toleration always")
	assert.Nil(t, pinned.NodeSelector, "a pinned pod keeps its node: the cache node may live outside the pool")

	free := &corev1.PodSpec{NodeSelector: map[string]string{"arch": "amd64"}}
	s.schedule(free)
	assert.Equal(t, map[string]string{"arch": "amd64", poolLabel: poolName}, free.NodeSelector, "an unpinned pod is routed onto the pool")

	unset := &corev1.PodSpec{}
	settings{}.schedule(unset)
	assert.Equal(t, corev1.PodSpec{}, *unset, "unset = unchanged")
}

func TestUntoleratedTaints(t *testing.T) {
	taints := []corev1.Taint{
		{Key: poolTaintKey, Effect: corev1.TaintEffectNoSchedule},
		{Key: "dedicated", Value: "llm", Effect: corev1.TaintEffectNoExecute},
		{Key: "soft", Effect: corev1.TaintEffectPreferNoSchedule},
	}
	assert.Equal(t, []string{"dedicated=llm:NoExecute", "nvidia.com/gpu:NoSchedule"}, settings{}.untolerated(taints), "hard taints only, key order")
	assert.Equal(t, []string{"dedicated=llm:NoExecute"}, settings{GPUPool: *poolInput()}.untolerated(taints))
	every := settings{GPUPool: backend.GPUPool{Taint: &backend.Taint{Key: "dedicated", Value: "llm"}}}
	assert.Equal(t, []string{"nvidia.com/gpu:NoSchedule"}, every.untolerated(taints), "Equal on the value, every effect")
	other := settings{GPUPool: backend.GPUPool{Taint: &backend.Taint{Key: "dedicated", Value: "web"}}}
	assert.Len(t, other.untolerated(taints), 2, "a different value tolerates nothing")
	assert.Empty(t, settings{}.untolerated(nil))
}

func TestEligibilityNamesTheTaintAndThePoolSelector(t *testing.T) {
	n := nodeBudget{Name: poolNode, Ready: true, Labels: map[string]string{poolLabel: poolName}, Taints: taintedPoolNode().Spec.Taints}
	ok, why := eligibility(n, settings{}, cacheLocation{})
	assert.False(t, ok)
	assert.Equal(t, "taint nvidia.com/gpu:NoSchedule not tolerated (set the GPU pool taint)", why)
	ok, why = eligibility(n, settings{GPUPool: *poolInput()}, cacheLocation{})
	assert.True(t, ok, why)
	outside := nodeBudget{Name: "gpu1", Ready: true, Labels: map[string]string{"accelerator": "gpu"}}
	ok, why = eligibility(outside, settings{GPUPool: *poolInput()}, cacheLocation{})
	assert.False(t, ok)
	assert.Equal(t, "outside the GPU pool node selector (giantswarm.io/machine-pool=c1-gpu01)", why)
}

// The composed predictors, the download Job, the scan pod and the descriptor
// carry the pool's toleration and label once discovery publishes them; the
// preset's own scheduling still merges on top.
func TestGPUPoolFromDiscoveryReachesEverything(t *testing.T) {
	f := newFixture(t, taintedPoolNode())
	ctx := context.Background()

	// Unset: nothing changes and the tainted node is reported as blocked.
	s := f.b.cfg.settings(ctx)
	obj := f.b.composeLLM(mustPreset(t, f, "big"), s, testGPUNode, referenceShape(mustPreset(t, f, "big")))
	_, has, _ := unstructured.NestedSlice(obj.Object, "spec", "predictor", "tolerations")
	assert.False(t, has, "no tolerations without an input")
	job := f.b.buildJob(downloadPlan{Dir: "big", Repo: bigRepo, Node: testCacheNode}, s)
	assert.Empty(t, job.Spec.Template.Spec.Tolerations)
	assert.Nil(t, f.b.Info(ctx).GPUPool)
	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	byName := map[string]backend.NodeInfo{}
	for _, n := range nodes {
		byName[n.Name] = n
	}
	require.Contains(t, byName, poolNode, "a tainted GPU node is listed")
	assert.False(t, byName[poolNode].Eligible)
	assert.Equal(t, "taint nvidia.com/gpu:NoSchedule not tolerated (set the GPU pool taint)", byName[poolNode].EligibilityReason)

	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	s = f.b.cfg.settings(ctx)
	want := map[string]any{"key": poolTaintKey, "operator": "Exists", "effect": "NoSchedule"}

	// The workload: pool toleration, pool label under the preset's selector and the pin.
	obj = f.b.composeLLM(mustPreset(t, f, "big"), s, poolNode, referenceShape(mustPreset(t, f, "big")))
	tols, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "tolerations")
	assert.Equal(t, []any{want}, tols)
	sel, _, _ := unstructured.NestedMap(obj.Object, "spec", "template", "nodeSelector")
	assert.Equal(t, map[string]any{poolLabel: poolName, "accelerator": "gpu", labelHostname: poolNode}, sel)

	// The preset's tolerations follow the pool's, a repeated entry once.
	custom := presetDoc("custom", "org/custom", 1, poolToleranceYAML)
	_, err = f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, presetConfigMap("custom", custom), metav1.CreateOptions{})
	require.NoError(t, err)
	obj = f.b.composeLLM(mustPreset(t, f, "custom"), s, "", referenceShape(mustPreset(t, f, "custom")))
	loadLLMISVCSchema(t).assertValid(t, obj)
	tols, _, _ = unstructured.NestedSlice(obj.Object, "spec", "template", "tolerations")
	assert.Equal(t, []any{want, map[string]any{"key": "dedicated", "operator": "Equal", "value": "llm", "effect": "NoExecute"}}, tols)
	sel, _, _ = unstructured.NestedMap(obj.Object, "spec", "template", "nodeSelector")
	assert.Equal(t, map[string]any{poolLabel: poolName}, sel, "no pin: the pool label alone")

	// The download Job (pinned to the cache node) and the scan pod.
	job = f.b.buildJob(downloadPlan{Dir: "big", Repo: bigRepo, Node: testCacheNode}, s)
	assert.Equal(t, s.poolTolerations(), job.Spec.Template.Spec.Tolerations)
	assert.Nil(t, job.Spec.Template.Spec.NodeSelector, "pinned: the cache node decides")
	pod := f.b.cachePod("scan", s, "", "true", true)
	assert.Equal(t, s.poolTolerations(), pod.Spec.Tolerations)
	assert.Equal(t, map[string]string{poolLabel: poolName}, pod.Spec.NodeSelector, "unpinned: onto the pool")

	// The descriptor reports the input; the pool node is capacity, the others are outside the pool.
	require.NotNil(t, f.b.Info(ctx).GPUPool)
	assert.Equal(t, poolInput(), f.b.Info(ctx).GPUPool)
	nodes, err = f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		byName[n.Name] = n
	}
	assert.True(t, byName[poolNode].Eligible, byName[poolNode].EligibilityReason)
	assert.Equal(t, "outside the GPU pool node selector (giantswarm.io/machine-pool=c1-gpu01)", byName[testGPUNode].EligibilityReason)
}

// A registered document's gpuPool replaces discovery's, field by field.
func TestGPUPoolOptionOverridesDiscovery(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.b.opts.GPUPool = backend.GPUPool{Taint: &backend.Taint{Key: "dedicated", Value: "llm", Effect: "NoExecute"}}
	f.b.cfg.opts.GPUPool = f.b.opts.GPUPool
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	s := f.b.cfg.settings(ctx)
	assert.Equal(t, &backend.Taint{Key: "dedicated", Value: "llm", Effect: "NoExecute"}, s.GPUPool.Taint, "the document's taint")
	assert.Equal(t, map[string]string{poolLabel: poolName}, s.GPUPool.NodeSelector, "discovery's selector stays")
}

// TestFitCheckPoolScalesFromZero: the pool selector names a pool no node
// belongs to yet (a Karpenter pool at scale-to-zero). The fit check answers
// yes without a node and says so; Load creates the serving object, whose
// pending predictor is what brings the node. An explicit node keeps the
// refusal, and once a pool node exists the fit is against that node.
func TestFitCheckPoolScalesFromZero(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput(), redirectPolicy: false})

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Contains(t, res.Reason, "no node in the GPU pool yet ("+poolLabel+"="+poolName+"): the pool scales from zero")
	assert.Contains(t, res.Reason, "unverified")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits, "an explicit node outside the pool is refused as before")
	assert.Contains(t, res.Reason, "outside the GPU pool node selector")

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	llmisvcs, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, llmisvcs.Items, 1, "the serving object exists; its pending workload brings the pool's node")
	selector, _, _ := unstructured.NestedMap(llmisvcs.Items[0].Object, "spec", "template", "nodeSelector")
	assert.NotContains(t, selector, labelHostname, "no node pin: the pool decides")

	// With a node in the pool the fit is against that node again.
	_, err = f.cs.CoreV1().Nodes().Create(ctx, taintedPoolNode(), metav1.CreateOptions{})
	require.NoError(t, err)
	f.b.inv.invalidate()
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.Equal(t, poolNode, res.Node)
	assert.NotEqual(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
}

// startingPoolNode is the pool's node right after its launch: created age
// ago, not ready yet, carrying the CSI agent's start-up taint beside the
// pool's own.
func startingPoolNode(age time.Duration) *corev1.Node {
	n := taintedPoolNode()
	n.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	n.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}}
	n.Spec.Taints = append(n.Spec.Taints,
		corev1.Taint{Key: "ebs.csi.aws.com/agent-not-ready", Effect: corev1.TaintEffectNoExecute},
		corev1.Taint{Key: "node.kubernetes.io/not-ready", Effect: corev1.TaintEffectNoSchedule})
	return n
}

// A node is starting while it is young and only start-up taints and its
// readiness keep it from serving; any other reason, or an old node, is not.
func TestStartingNodeCarriesOnlyStartupTaints(t *testing.T) {
	now := time.Now()
	s := settings{GPUPool: *poolInput()}
	young := budgetOf(startingPoolNode(2*time.Minute), DefaultGPUResourceName, "")
	young.Eligible, _ = eligibility(young, s, cacheLocation{})
	require.False(t, young.Eligible)
	assert.True(t, starting(young, s, cacheLocation{}, now), "start-up taints and not ready on a young node")

	old := budgetOf(startingPoolNode(time.Hour), DefaultGPUResourceName, "")
	assert.False(t, starting(old, s, cacheLocation{}, now), "past the start-up window the node is broken, not starting")

	other := budgetOf(startingPoolNode(time.Minute), DefaultGPUResourceName, "")
	other.Taints = append(other.Taints, corev1.Taint{Key: "dedicated", Value: "web", Effect: corev1.TaintEffectNoSchedule})
	assert.False(t, starting(other, s, cacheLocation{}, now), "a taint the pool does not tolerate is no start-up taint")

	gone := budgetOf(startingPoolNode(time.Minute), DefaultGPUResourceName, "")
	gone.Taints = append(gone.Taints, corev1.Taint{Key: "node.kubernetes.io/unreachable", Effect: corev1.TaintEffectNoExecute})
	assert.False(t, starting(gone, s, cacheLocation{}, now), "an unreachable node went away")

	assert.False(t, starting(young, settings{}, cacheLocation{}, now), "without the pool's toleration the pool taint stays a reason")
}

// TestFitCheckWaitsForAStartingPoolNode: the pool's only node was just
// launched and still carries its start-up taints (giantswarm/model-manager#243).
// The fit is the pool's, as at scale-from-zero, naming the node it waits
// for; Load creates the serving object without a node pin. An old node with
// the same taints keeps the refusal.
func TestFitCheckWaitsForAStartingPoolNode(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, startingPoolNode(2*time.Minute))
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput(), redirectPolicy: false})

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Contains(t, res.Reason, "no ready node in the GPU pool yet ("+poolLabel+"="+poolName+"): node "+poolNode+" is starting (not ready, ebs.csi.aws.com/agent-not-ready:NoExecute, node.kubernetes.io/not-ready:NoSchedule)")

	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		if n.Name == poolNode {
			assert.False(t, n.Eligible)
			assert.Contains(t, n.EligibilityReason, "; starting (created 2m")
		}
	}

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	llmisvcs, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, llmisvcs.Items, 1, "the serving object exists and waits for the starting node")
	selector, _, _ := unstructured.NestedMap(llmisvcs.Items[0].Object, "spec", "template", "nodeSelector")
	assert.NotContains(t, selector, labelHostname, "no node pin: the pool decides")
	assert.Equal(t, poolName, selector[poolLabel])

	stale := newFixture(t, startingPoolNode(time.Hour))
	stale.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput(), redirectPolicy: false})
	res, err = stale.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits, "an hour-old node is not starting")
	assert.Contains(t, res.Reason, "no eligible node: ")
}

// Without a GPU pool configured, the cluster's only accelerator node right
// after its launch is waited for too: the reason names it, Load proceeds.
func TestFitCheckWaitsForAStartingNodeWithoutAPool(t *testing.T) {
	ctx := context.Background()
	n := startingPoolNode(time.Minute)
	n.Spec.Taints = n.Spec.Taints[1:] // only the start-up taints, no pool taint
	f := newFixture(t, n)
	for _, name := range []string{testGPUNode, testCacheNode} {
		require.NoError(t, f.cs.CoreV1().Nodes().Delete(ctx, name, metav1.DeleteOptions{}))
	}
	f.setDiscoveryOpts(ctx, discoveryOpts{redirectPolicy: false})

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Contains(t, res.Reason, "no ready node yet: node "+poolNode+" is starting (not ready, ebs.csi.aws.com/agent-not-ready:NoExecute, node.kubernetes.io/not-ready:NoSchedule)")
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: poolNode})
	require.NoError(t, err)
	assert.False(t, res.Fits, "an explicit node is judged as it is")
}
