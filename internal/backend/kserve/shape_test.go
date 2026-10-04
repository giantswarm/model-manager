package kserve

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/giantswarm/model-manager/internal/backend"
)

// fourCardPreset is a preset written for four 48 GB L40S cards, as the
// shipped gpt-oss-120b (61 GiB of weights, 40 GiB overhead) and
// mistral-small-4 (66 GiB, 60 GiB) are.
func fourCardPreset(name string, weightsGiB, overheadGiB float64) *corev1.ConfigMap {
	return presetConfigMap(name, fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %[1]s
spec:
  model:
    id: example/%[1]s
    storageUri: oci://registry.example/models/%[1]s:1
  args:
    - --max-model-len=65536
    - --tensor-parallel-size=4
    - --gpu-memory-utilization=0.90
  resources:
    gpus: 4
    requests: {cpu: "16", memory: 96Gi}
  requirements:
    weightsGiB: %[2]v
    overheadGiB: %[3]v
`, name, weightsGiB, overheadGiB))
}

// g6e12xlarge is a g6e.12xlarge: four 48 GB L40S cards, 48 vCPUs.
func g6e12xlarge(name string) *corev1.Node {
	return withCPUs(withGPUs(node(name, "360Gi", map[string]string{labelGPUPresent: "true", labelGPUCount: "4", labelGPUMemory: "46068", labelGPUProduct: "NVIDIA-L40S"}), 4), "48")
}

// spark is a GB10 node with its 20 cores.
func spark(name string) *corev1.Node { return withCPUs(gb10Node(name), "20") }

func withCPUs(n *corev1.Node, cpus string) *corev1.Node {
	n.Status.Allocatable[corev1.ResourceCPU] = resource.MustParse(cpus)
	return n
}

// systemPod is a running pod on node requesting cpu.
func systemPod(name, node, cpu string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "kube-system"},
		Spec:       corev1.PodSpec{NodeName: node, Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func shapeFixture(t *testing.T, extra ...runtime.Object) *fixture {
	t.Helper()
	objs := append([]runtime.Object{fourCardPreset("gpt-oss-120b", 61, 40), fourCardPreset("mistral-small-4", 66, 60)}, extra...)
	f := newFixture(t, objs...)
	f.b.cfg.opts.FastLinks = []backend.FastLink{{Name: "sparks", Nodes: []string{"gb10-a", "gb10-b"}}}
	f.resetSettings()
	return f
}

func dryRun(t *testing.T, f *fixture, req backend.LoadRequest) *unstructured.Unstructured {
	t.Helper()
	req.DryRun = true
	res, err := f.b.Serve(context.Background(), req)
	require.NoError(t, err)
	require.Len(t, res.Manifests, 1)
	return res.Manifests[0]
}

func templateArgs(tpl map[string]any) []string {
	var out []string
	for _, a := range templateMain(tpl)["args"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func templateGPUs(tpl map[string]any) any {
	res := templateMain(tpl)["resources"].(map[string]any)
	return res["requests"].(map[string]any)[DefaultGPUResourceName]
}

// The presets written for four L40S cards load unchanged on a GB10: one
// device per pod, tensor parallel 1, the utilization sized to their weights
// and overhead.
func TestShapeOnAUnifiedMemoryNode(t *testing.T) {
	f := shapeFixture(t, spark("gb10-a"), spark("gb10-b"))
	ctx := context.Background()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-120b", Node: "gb10-a"})
	require.NoError(t, err)
	require.True(t, res.Fits, res.Reason)
	assert.Equal(t, int64(1), res.DevicesPerPod)
	assert.Equal(t, int64(1), res.TensorParallel)
	assert.InDelta(t, 0.83, res.GPUMemoryUtilization, 1e-9)
	assert.Contains(t, res.Reason, "runs with 1 GPU device per pod and tensor parallel 1 there (the preset's reference shape: 4 GPU devices, tensor parallel 4)")

	obj := dryRun(t, f, backend.LoadRequest{Preset: "gpt-oss-120b", Node: "gb10-a"})
	tpl := obj.Object["spec"].(map[string]any)["template"].(map[string]any)
	assert.Equal(t, "1", templateGPUs(tpl))
	args := strings.Join(templateArgs(tpl), " ")
	assert.Contains(t, args, "--tensor-parallel-size 1")
	assert.Contains(t, args, "--gpu-memory-utilization 0.83")
	assert.NotContains(t, args, "--tensor-parallel-size=4")
	assert.NotContains(t, args, "0.90")
}

// A split of both presets across two GB10s requests one device on each pod
// and runs tensor parallel over the two.
func TestShapeOfASplitAcrossUnifiedMemoryNodes(t *testing.T) {
	f := shapeFixture(t, spark("gb10-a"), spark("gb10-b"))
	ctx := context.Background()

	for preset, utilization := range map[string]string{"gpt-oss-120b": "0.58", "mistral-small-4": "0.77"} {
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: preset, Placement: backend.PlacementSplit})
		require.NoError(t, err)
		require.True(t, res.Fits, res.Reason)
		assert.Equal(t, int64(1), res.DevicesPerPod, preset)
		assert.Equal(t, int64(2), res.TensorParallel, preset)
		assert.Contains(t, res.Reason, "--gpu-memory-utilization="+utilization+", sized down from the preset's 0.9", preset)

		obj := dryRun(t, f, backend.LoadRequest{Preset: preset, Placement: backend.PlacementSplit})
		spec := obj.Object["spec"].(map[string]any)
		for _, role := range []string{"template", "worker"} {
			tpl := spec[role].(map[string]any)
			assert.Equal(t, "1", templateGPUs(tpl), preset+" "+role)
			assert.Contains(t, templateMain(tpl)["command"].([]any)[2], "--tensor-parallel-size 2 --nnodes 2", preset+" "+role)
			args := strings.Join(templateArgs(tpl), " ")
			assert.NotContains(t, args, "--tensor-parallel-size", preset+" "+role)
			assert.Contains(t, args, "--gpu-memory-utilization "+utilization, preset+" "+role)
		}
	}
}

// On the four-card node the presets are written for they render as written:
// four devices, tensor parallel 4, their own utilization.
func TestShapeOnTheReferenceNode(t *testing.T) {
	f := shapeFixture(t, g6e12xlarge("g6e"))
	ctx := context.Background()

	for _, preset := range []string{"gpt-oss-120b", "mistral-small-4"} {
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: preset, Node: "g6e"})
		require.NoError(t, err)
		require.True(t, res.Fits, res.Reason)
		assert.Equal(t, int64(4), res.DevicesPerPod, preset)
		assert.Equal(t, int64(4), res.TensorParallel, preset)
		assert.InDelta(t, 0.9, res.GPUMemoryUtilization, 1e-9, preset)
		assert.NotContains(t, res.Reason, "reference shape", preset)

		obj := dryRun(t, f, backend.LoadRequest{Preset: preset, Node: "g6e"})
		tpl := obj.Object["spec"].(map[string]any)["template"].(map[string]any)
		assert.Equal(t, "4", templateGPUs(tpl), preset)
		assert.Equal(t, []string{"--max-model-len=65536", "--tensor-parallel-size=4", "--gpu-memory-utilization=0.90"}, templateArgs(tpl)[:3], preset)
	}
}

// check_fit refuses a placement whose devices per pod the node cannot
// schedule, naming them.
func TestShapeRefusesMoreDevicesThanAllocatable(t *testing.T) {
	n := g6e12xlarge("half")
	withGPUs(n, 2)
	f := shapeFixture(t, n)

	res, err := f.b.FitCheck(context.Background(), backend.FitRequest{Preset: "gpt-oss-120b", Node: "half"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Equal(t, int64(4), res.DevicesPerPod)
	assert.Contains(t, res.Reason, "each serving pod needs 4 GPU devices there, and half has 2 allocatable")
}

// Copies share one pod template: nodes that need different device counts
// cannot carry copies of one model.
func TestCopiesRefuseNodesOfDifferentShapes(t *testing.T) {
	f := shapeFixture(t, spark("gb10-a"), g6e12xlarge("g6e"))

	res, err := f.b.FitCheck(context.Background(), backend.FitRequest{Preset: "gpt-oss-120b", Placement: backend.PlacementCopies, Nodes: []string{"gb10-a", "g6e"}})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "the nodes need different serving shapes (g6e: 4 GPU devices, tensor parallel 4; gb10-a: 1 GPU device, tensor parallel 1)")
}

func TestDevicesOn(t *testing.T) {
	p := &servingPreset{}
	p.Spec.Resources.GPUs = new(int64)
	*p.Spec.Resources.GPUs = 4
	l40s := nodeBudget{GPUCount: 8, GPUMemory: 46068 * mib}
	for _, tc := range []struct {
		name  string
		node  nodeBudget
		share int64
		want  int64
	}{
		{"unified", nodeBudget{GPUCount: 1}, 101 * gib, 1},
		{"several GPUs without memory labels keep the reference", nodeBudget{GPUCount: 4}, 101 * gib, 4},
		{"one card holds it", l40s, 30 * gib, 1},
		{"three cards round up to four", l40s, 101 * gib, 4},
		{"two cards", l40s, 60 * gib, 2},
		{"five cards round up to eight", l40s, 200 * gib, 8},
	} {
		assert.Equal(t, tc.want, devicesOn(p, tc.node, tc.share), tc.name)
	}
}

// A preset's CPU request is its reference node's: on a GB10 whose other pods
// already request 5.4 of its 20 cores, the pod requests the 14.6 left; with
// none left, the fit is refused.
func TestShapeCapsRequestsAtWhatTheNodeHasLeft(t *testing.T) {
	f := shapeFixture(t, spark("gb10-a"), spark("gb10-b"), systemPod("busy", "gb10-a", "5400m"), systemPod("full", "gb10-b", "20"))
	ctx := context.Background()

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-120b", Node: "gb10-a"})
	require.NoError(t, err)
	require.True(t, res.Fits, res.Reason)
	assert.Equal(t, int64(14600), res.CPURequestMillis)
	assert.Equal(t, gibToBytes(96), res.MemoryRequestBytes)
	assert.Contains(t, res.Reason, "requests 14600m CPU there, capped from the preset's 16 to what gb10-a has left")

	obj := dryRun(t, f, backend.LoadRequest{Preset: "gpt-oss-120b", Node: "gb10-a"})
	requests := templateMain(obj.Object["spec"].(map[string]any)["template"].(map[string]any))["resources"].(map[string]any)["requests"].(map[string]any)
	assert.Equal(t, "14600m", requests["cpu"])
	assert.Equal(t, "96Gi", requests["memory"], "a request the node holds stays as written")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-120b", Node: "gb10-b"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "each serving pod requests 16 CPU and gb10-b has 0 left beside the requests of its other pods")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "gpt-oss-120b", Placement: backend.PlacementSplit})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Contains(t, res.Reason, "gb10-b has 0 left")
}
