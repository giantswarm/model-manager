package kserve

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// A preset that requests no GPU (resources.gpus: 0) is CPU work. The fit
// judges every node against its allocatable memory and knows no GPU pool: the
// pool's taint keeps the accelerator nodes out, the CPU node is the target,
// and an explicit accelerator node is refused with the taint as the reason.
// The LLMInferenceService it composes requests no accelerator and carries
// neither the pool's toleration and selector nor the accelerator RuntimeClass.
// A preset that requests a GPU keeps the accelerator nodes as its capacity,
// tolerates the pool and runs under the RuntimeClass, and the nodes API still
// lists the accelerator nodes alone.
func TestCPUPresetServesOnCPUCapacity(t *testing.T) {
	ctx := context.Background()
	cpuPreset := strings.Replace(presetDoc("cpu-tiny", presetlessRepo, 0.5, ""), "gpus: 1", "gpus: 0", 1)
	f := newFixture(t, presetConfigMap("cpu-tiny", cpuPreset))
	f.serveLLMAPI()
	// The GPU pool's nodes carry its taint and its label, as the pool chart
	// leaves them; the CPU node carries neither.
	for _, name := range []string{testCacheNode, testGPUNode} {
		n, err := f.cs.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, err)
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: DefaultGPUResourceName, Effect: corev1.TaintEffectNoSchedule})
		n.Labels["pool"] = "gpu"
		_, err = f.cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	f.setDiscoveryOpts(ctx, discoveryOpts{
		gpuPool:          &backend.GPUPool{Taint: &backend.Taint{Key: DefaultGPUResourceName, Effect: string(corev1.TaintEffectNoSchedule)}, NodeSelector: map[string]string{"pool": "gpu"}},
		runtimeClassName: "nvidia",
	})

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "cpu-tiny"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testCPUNode, res.Node, "the CPU node is the target: the pool's taint keeps the accelerator nodes out")
	assert.Equal(t, budgetSourceAllocatable, res.BudgetSource, "CPU work is bounded by the node's memory")
	assert.EqualValues(t, int64(32)<<30, res.BudgetBytes)

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "cpu-tiny", Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, "node gpu1 is not a serving target: taint nvidia.com/gpu:NoSchedule not tolerated (set the GPU pool taint)", res.Reason)

	// The GPU preset keeps the pool: its nodes, tolerated, the cache node first.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "tiny"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testCacheNode, res.Node)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "tiny", Node: testCPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Contains(t, res.Reason, `node "cpu1" not found: it does not exist or is not an accelerator node`)

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "cpu-tiny"}))
	obj, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "cpu-tiny", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "hf://"+presetlessRepo, mustNested(t, obj.Object, "spec", "model", "uri"))
	main := mainContainer(obj)
	require.NotNil(t, main)
	requests, _, _ := unstructured.NestedMap(main, "resources", "requests")
	assert.NotContains(t, requests, DefaultGPUResourceName, "no accelerator requested")
	limits, _, _ := unstructured.NestedMap(main, "resources", "limits")
	assert.NotContains(t, limits, DefaultGPUResourceName)
	_, has, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "tolerations")
	assert.False(t, has, "no pool toleration")
	_, has, _ = unstructured.NestedMap(obj.Object, "spec", "template", "nodeSelector")
	assert.False(t, has, "no pool selector")
	_, has, _ = unstructured.NestedString(obj.Object, "spec", "template", "runtimeClassName")
	assert.False(t, has, "no accelerator RuntimeClass")

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	obj, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "nvidia", mustNested(t, obj.Object, "spec", "template", "runtimeClassName"))
	tolerations, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "tolerations")
	assert.Len(t, tolerations, 1, "the pool's toleration")
	selector, _, _ := unstructured.NestedMap(obj.Object, "spec", "template", "nodeSelector")
	assert.Equal(t, "gpu", selector["pool"])
	requests, _, _ = unstructured.NestedMap(mainContainer(obj), "resources", "requests")
	assert.Equal(t, "1", requests[DefaultGPUResourceName])

	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}
	assert.Equal(t, []string{testGPUNode, testCacheNode}, names, "the nodes API lists the accelerator nodes")
}
