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

// copiesFixture is splitFixture without a fast link, plus a preset served
// from a model image that one GB10 node holds whole.
func copiesFixture(t *testing.T) *fixture {
	t.Helper()
	f := splitFixture(t)
	mid := strings.Replace(presetDoc("mid", "org/mid", 40, ""), "storageUri: hf://org/mid", "storageUri: oci://registry.example/models/mid:1", 1)
	_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(context.Background(), presetConfigMap("mid", mid), metav1.CreateOptions{})
	require.NoError(t, err)
	f.resetSettings()
	return f
}

func fitsByNode(copies []backend.NodeFit) map[string]backend.NodeFit {
	out := map[string]backend.NodeFit{}
	for _, c := range copies {
		out[c.Node] = c
	}
	return out
}

func TestCopiesFitJudgesEachNode(t *testing.T) {
	ctx := context.Background()
	f := copiesFixture(t)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "mid", Placement: backend.PlacementCopies, Nodes: []string{sparkA, sparkB}})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, backend.PlacementCopies, res.Placement)
	assert.Equal(t, []string{sparkA, sparkB}, res.Nodes)
	assert.Contains(t, res.Reason, "2 copies on spark-a, spark-b")
	assert.Equal(t, res.WeightsBytes+res.OverheadBytes, res.RequiredBytes, "each copy holds the whole model")
	by := fitsByNode(res.Copies)
	assert.True(t, by[sparkA].Fits, by[sparkA].Reason)
	assert.True(t, by[sparkB].Fits, by[sparkB].Reason)

	one, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "mid"})
	require.NoError(t, err)
	assert.True(t, one.Fits, one.Reason)
	assert.Empty(t, one.Nodes, "one copy on the node the placement picks")
	by = fitsByNode(one.Copies)
	assert.True(t, by[sparkA].Fits, "every node's verdict of one copy: %+v", one.Copies)
	assert.True(t, by[sparkB].Fits)
	assert.Equal(t, backend.PlacementCopies, one.Recommended, "no fast link: copies")

	pinned, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "mid", Placement: backend.PlacementCopies, Nodes: []string{sparkB}})
	require.NoError(t, err)
	assert.Equal(t, sparkB, pinned.Node, "copies on one node are the node pin")
	assert.Empty(t, pinned.Copies)

	refused, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "mid", Placement: backend.PlacementCopies, Nodes: []string{sparkA, testCPUNode}})
	require.NoError(t, err)
	assert.False(t, refused.Fits)
	assert.Contains(t, refused.Reason, "1 of 2 nodes cannot host a copy — cpu1: ")
	assert.Equal(t, testCPUNode, refused.Node, "the figures are the refusing node's")
	by = fitsByNode(refused.Copies)
	assert.True(t, by[sparkA].Fits)
	assert.False(t, by[testCPUNode].Fits)

	huge, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge", Placement: backend.PlacementCopies, Nodes: []string{sparkA, sparkB}})
	require.NoError(t, err)
	assert.False(t, huge.Fits, "a copy holds the whole model, more than one node has")
	assert.Contains(t, huge.Reason, "2 of 2 nodes cannot host a copy")
}

func TestServeCopiesPinsOneReplicaPerNode(t *testing.T) {
	ctx := context.Background()
	f := copiesFixture(t)

	res, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "mid", Placement: backend.PlacementCopies, Nodes: []string{sparkA, sparkB}})
	require.NoError(t, err)
	assert.False(t, res.AlreadyServing)
	assert.Equal(t, []string{sparkA, sparkB}, res.Fit.Nodes)

	obj := &unstructured.Unstructured{Object: f.llmisvc(ctx, "mid")}
	assertCopies(t, obj, sparkA, sparkB)

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PlacementCopies, loaded[0].Placement)
	assert.Equal(t, []string{sparkA, sparkB}, loaded[0].Nodes)

	// Each copy holds its node: another model is judged against what is left.
	for _, n := range []string{sparkA, sparkB} {
		other, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge", Node: n})
		require.NoError(t, err)
		assert.Positive(t, other.ReservedBytes, "%s: the copy there is reserved", n)
	}

	again, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "mid", Placement: backend.PlacementCopies, Nodes: []string{sparkB, sparkA}})
	require.NoError(t, err)
	assert.True(t, again.AlreadyServing)
	assert.Equal(t, []string{sparkA, sparkB}, again.ServingNodes)
}

func TestServeOnMoreNodesAddsCopies(t *testing.T) {
	ctx := context.Background()
	f := copiesFixture(t)

	_, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "mid", Node: sparkA})
	require.NoError(t, err)

	dry, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "mid", Node: sparkB, DryRun: true})
	require.NoError(t, err)
	require.Len(t, dry.Manifests, 1)
	assertCopies(t, dry.Manifests[0], sparkA, sparkB)
	unchanged := &unstructured.Unstructured{Object: f.llmisvc(ctx, "mid")}
	replicas, _, _ := unstructured.NestedInt64(unchanged.Object, "spec", "replicas")
	assert.Equal(t, int64(1), replicas, "a dry run changes nothing")

	res, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "mid", Placement: backend.PlacementCopies, Nodes: []string{sparkB}})
	require.NoError(t, err)
	assert.False(t, res.AlreadyServing, "a copy was added, not an unchanged success")
	assert.Equal(t, []string{sparkA, sparkB}, res.ServingNodes)
	assertCopies(t, &unstructured.Unstructured{Object: f.llmisvc(ctx, "mid")}, sparkA, sparkB)

	res, err = f.b.Serve(ctx, backend.LoadRequest{Preset: "mid", Node: sparkB})
	require.NoError(t, err)
	assert.True(t, res.AlreadyServing)
}

func TestSplitReservesItsShareOnEveryNode(t *testing.T) {
	ctx := context.Background()
	f := copiesFixture(t)
	f.b.cfg.opts.FastLinks = []backend.FastLink{sparkLink}
	f.resetSettings()

	_, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "huge", Placement: backend.PlacementSplit})
	require.NoError(t, err)
	for _, n := range []string{sparkA, sparkB} {
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "mid", Node: n})
		require.NoError(t, err)
		assert.Positive(t, res.ReservedBytes, "%s holds its share of the split", n)
	}
}

func assertCopies(t *testing.T, obj *unstructured.Unstructured, nodes ...string) {
	t.Helper()
	assert.Equal(t, backend.PlacementCopies, obj.GetAnnotations()[PlacementAnnotation])
	assert.Equal(t, strings.Join(nodes, ","), obj.GetAnnotations()[NodesAnnotation])
	replicas, _, _ := unstructured.NestedInt64(obj.Object, "spec", "replicas")
	assert.Equal(t, int64(len(nodes)), replicas, "one replica per node")
	_, pinned, _ := unstructured.NestedString(obj.Object, "spec", "template", "nodeSelector", labelHostname)
	assert.False(t, pinned, "the copies are placed by affinity, not one node's selector")
	terms, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	require.Len(t, terms, 1)
	assert.Equal(t, toAnySlice(nodes), terms[0].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)["values"])
	anti, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "affinity", "podAntiAffinity", "requiredDuringSchedulingIgnoredDuringExecution")
	require.Len(t, anti, 1)
	labels := anti[0].(map[string]any)["labelSelector"].(map[string]any)["matchLabels"].(map[string]any)
	assert.Equal(t, workloadComponent, labels["app.kubernetes.io/component"])
	assert.Equal(t, obj.GetName(), labels["app.kubernetes.io/name"])
}
