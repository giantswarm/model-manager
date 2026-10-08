package kserve

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// devicesFixture is a GPU pool whose only node is n, with two one-GPU
// presets whose weights leave room for both in the node's memory budget, and
// preset "one" served there: its predictor's pod runs on the node.
func devicesFixture(t *testing.T, n *corev1.Node, shapes ...backend.InstanceShape) *fixture {
	t.Helper()
	ctx := context.Background()
	f := newFixture(t, n, presetConfigMap("one", presetDoc("one", fatRepo, 10, "")), presetConfigMap("two", presetDoc("two", fatRepo, 10, "")))
	f.setPool(ctx, shapes...)
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "one"}))
	pod := workloadPod("one", corev1.PodRunning)
	pod.Spec.NodeName = n.Name
	_, err := f.cs.CoreV1().Pods(testServingNS).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	return f
}

// A one-GPU node whose device a running predictor holds is no placement for
// a second model, whatever memory its budget has left: the verdict names the
// devices; the model holding it is still judged on it (its own GPU is not
// taken), and list_nodes shows the node's free devices
// (giantswarm/model-manager#257).
func TestFitCheckRefusesANodeWhoseGPUIsTaken(t *testing.T) {
	ctx := context.Background()
	f := devicesFixture(t, l40sPoolNode())

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "two"})
	require.NoError(t, err)
	assert.False(t, res.Fits, res.Reason)
	assert.Equal(t, poolNode, res.Node)
	assert.Positive(t, res.FreeBytes, "the memory budget alone would take it")
	require.NotNil(t, res.FreeGPUs)
	assert.Zero(t, *res.FreeGPUs)
	assert.Contains(t, res.Reason, "node "+poolNode+" has 0 GPUs free of its 1 GPU (1 GPU taken by running models), the predictor requests 1")
	require.ErrorIs(t, f.b.Load(ctx, backend.LoadRequest{Preset: "two"}), backend.ErrUnfit)

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "one"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	require.NotNil(t, res.FreeGPUs)
	assert.EqualValues(t, 1, *res.FreeGPUs, "the model's own predictor holds no device against itself")

	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		if n.Name == poolNode {
			require.NotNil(t, n.FreeGPUs)
			assert.Zero(t, *n.FreeGPUs)
		}
	}
}

// A pool whose only node has its GPU taken launches one of its sizes for the
// next model: the node takes no predictor (takes).
func TestFitCheckLaunchesAPoolNodeWhenTheGPUIsTaken(t *testing.T) {
	ctx := context.Background()
	f := devicesFixture(t, l40sPoolNode(), shapeL40S2XLarge)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "two"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Equal(t, "g6e.2xlarge", res.InstanceType)
	assert.Contains(t, res.Reason, "node "+poolNode+" has 0 of 1 GPU free, the predictor requests 1")
}

// A two-GPU node with one device taken hosts the next one-GPU model as
// before.
func TestFitCheckPlacesOnAFreeGPUOfAMultiGPUNode(t *testing.T) {
	ctx := context.Background()
	n := l40sPoolNode()
	n.Labels[labelGPUCount] = "2"
	f := devicesFixture(t, withGPUs(n, 2))

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "two"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, poolNode, res.Node)
	require.NotNil(t, res.FreeGPUs)
	assert.EqualValues(t, 1, *res.FreeGPUs)
}

// A node whose GPUs report no memory of their own (unified memory) keeps the
// memory judgement alone: no device count, none taken.
func TestFitCheckKeepsTheMemoryJudgementWithoutDiscreteGPUs(t *testing.T) {
	ctx := context.Background()
	n := node(poolNode, "256Gi", map[string]string{poolLabel: poolName, labelGPUCount: "1", labelGPUProduct: "NVIDIA-GB10"})
	n.Spec.Taints = l40sPoolNode().Spec.Taints
	f := devicesFixture(t, withGPUs(n, 1))

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "two"})
	require.NoError(t, err)
	assert.Equal(t, poolNode, res.Node)
	assert.Nil(t, res.FreeGPUs)
	assert.NotContains(t, res.Reason, "taken by running models")
	assert.Equal(t, res.RequiredBytes <= res.FreeBytes, res.Fits, "the memory verdict: %s", res.Reason)
}
