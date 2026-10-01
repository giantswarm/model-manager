package kserve

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	sparkA = "spark-a"
	sparkB = "spark-b"
	// splitRepo is served by the "huge" preset from a model image: 200 GiB
	// of weights, more than one 128 GiB node holds, half of it on each.
	splitRepo = "org/huge"
)

var sparkLink = backend.FastLink{
	Name:      "sparks",
	Nodes:     []string{sparkA, sparkB},
	Networks:  []string{"roce-a", "roce-b"},
	Resources: map[string]string{"rdma/rdma_shared_device_a": "1"},
	Env:       []backend.EnvVar{{Name: "NCCL_IB_HCA", Value: "rocep1s0f1,roceP2p1s0f1"}},
}

// splitFixture has two unified-memory GB10 nodes joined by a fast link and a
// preset served from a model image that only a split hosts.
func splitFixture(t *testing.T, links ...backend.FastLink) *fixture {
	t.Helper()
	gb10 := map[string]string{"accelerator": "gpu", labelGPUCount: "1", labelGPUMemory: "131072", labelGPUProduct: "GB10"}
	huge := strings.Replace(presetDoc("huge", splitRepo, 200, ""), "storageUri: hf://"+splitRepo, "storageUri: oci://registry.example/models/huge:1", 1)
	huge = strings.Replace(huge, "    - --max-model-len=4096\n", "    - --max-model-len=4096\n    - --tensor-parallel-size=1\n", 1)
	giant := strings.Replace(presetDoc("giant", "org/giant", 400, ""), "storageUri: hf://org/giant", "storageUri: oci://registry.example/models/giant:1", 1)
	duo := strings.Replace(presetDoc("duo", "org/duo", 50, splitEnvDoc), "storageUri: hf://org/duo", "storageUri: oci://registry.example/models/duo:1", 1)
	f := newFixture(t,
		node(sparkA, "128Gi", gb10),
		node(sparkB, "128Gi", gb10),
		presetConfigMap("huge", huge),
		presetConfigMap("giant", giant),
		presetConfigMap("duo", duo),
	)
	f.b.cfg.opts.FastLinks = links
	f.resetSettings()
	return f
}

func TestValidatePlacement(t *testing.T) {
	for _, tc := range []struct {
		placement string
		nodes     []string
		want      string
		err       string
	}{
		{"", nil, backend.PlacementCopies, ""},
		{"copies", []string{"a"}, backend.PlacementCopies, ""},
		{"copies", []string{"a", "b"}, backend.PlacementCopies, ""},
		{"copies", []string{"a", "a"}, "", "named twice"},
		{"split", nil, backend.PlacementSplit, ""},
		{"split", []string{"a", "b"}, backend.PlacementSplit, ""},
		{"split", []string{"a"}, "", "two or more nodes"},
		{"split", []string{"a", "a"}, "", "named twice"},
		{"spread", nil, "", `placement "spread"`},
	} {
		got, err := validatePlacement(tc.placement, tc.nodes)
		if tc.err != "" {
			require.ErrorIs(t, err, backend.ErrInvalid, tc.placement)
			assert.Contains(t, err.Error(), tc.err)
			continue
		}
		require.NoError(t, err)
		assert.Equal(t, tc.want, got)
	}
}

func TestValidateFastLinks(t *testing.T) {
	require.NoError(t, backend.ValidateFastLinks([]backend.FastLink{sparkLink}))
	for _, tc := range []struct {
		links []backend.FastLink
		err   string
	}{
		{[]backend.FastLink{{Nodes: []string{"a", "b"}}}, "name: required"},
		{[]backend.FastLink{{Name: "x", Nodes: []string{"a"}}}, "two or more nodes"},
		{[]backend.FastLink{{Name: "x", Nodes: []string{"a", "b"}}, {Name: "y", Nodes: []string{"b", "c"}}}, "already in fast link x"},
		{[]backend.FastLink{{Name: "x", Nodes: []string{"a", "b"}, Env: []backend.EnvVar{{Value: "1"}}}}, "without a name"},
	} {
		err := backend.ValidateFastLinks(tc.links)
		require.Error(t, err)
		assert.Contains(t, err.Error(), tc.err)
	}
}

func TestSplitFitRecommendsTheFastLink(t *testing.T) {
	ctx := context.Background()
	f := splitFixture(t, sparkLink)

	copies, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge"})
	require.NoError(t, err)
	assert.False(t, copies.Fits, copies.Reason)
	assert.Equal(t, backend.PlacementCopies, copies.Placement)
	assert.Equal(t, backend.PlacementSplit, copies.Recommended, "one node does not hold it, the fast link does")
	assert.Equal(t, []string{sparkA, sparkB}, copies.RecommendedNodes)

	split, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge", Placement: backend.PlacementSplit})
	require.NoError(t, err)
	assert.True(t, split.Fits, split.Reason)
	assert.Equal(t, backend.PlacementSplit, split.Placement)
	assert.Equal(t, "sparks", split.FastLink)
	assert.Equal(t, []string{sparkA, sparkB}, split.Nodes)
	assert.Equal(t, sparkA, split.Node)
	assert.Equal(t, (split.WeightsBytes+1)/2+split.OverheadBytes, split.RequiredBytes, "a split's requirement is per node: half the weights and the overhead")
	assert.Contains(t, split.Reason, "split across spark-a, spark-b (fast link sparks)")
	assert.Equal(t, backend.PlacementSplit, split.Recommended)

	reversed, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge", Placement: backend.PlacementSplit, Nodes: []string{sparkB, sparkA}})
	require.NoError(t, err)
	assert.True(t, reversed.Fits, reversed.Reason)
	assert.Equal(t, []string{sparkB, sparkA}, reversed.Nodes, "the asked rank order stands")
}

func TestSplitRefusals(t *testing.T) {
	ctx := context.Background()

	t.Run("nodes outside one fast link", func(t *testing.T) {
		f := splitFixture(t, sparkLink)
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge", Placement: backend.PlacementSplit, Nodes: []string{sparkA, testGPUNode}})
		require.NoError(t, err)
		assert.False(t, res.Fits)
		assert.Contains(t, res.Reason, "share no fast link")
		_, err = f.b.Serve(ctx, backend.LoadRequest{Preset: "huge", Placement: backend.PlacementSplit, Nodes: []string{sparkA, testGPUNode}})
		require.ErrorIs(t, err, backend.ErrUnfit)
		assert.Contains(t, err.Error(), "share no fast link")
	})

	t.Run("no fast link on the cluster", func(t *testing.T) {
		f := splitFixture(t)
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "huge", Placement: backend.PlacementSplit})
		require.NoError(t, err)
		assert.False(t, res.Fits)
		assert.Contains(t, res.Reason, "no fast link joins nodes")
		assert.Equal(t, backend.PlacementCopies, res.Recommended)
	})

	t.Run("weights on a node-local cache", func(t *testing.T) {
		f := splitFixture(t, backend.FastLink{Name: "pair", Nodes: []string{testCacheNode, testGPUNode}})
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "tiny", Placement: backend.PlacementSplit})
		require.NoError(t, err)
		assert.False(t, res.Fits)
		assert.Contains(t, res.Reason, "cannot mount the cache claim")
	})

	t.Run("a share that does not fit", func(t *testing.T) {
		f := splitFixture(t, sparkLink)
		res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "giant", Placement: backend.PlacementSplit})
		require.NoError(t, err)
		assert.False(t, res.Fits)
		assert.Contains(t, res.Reason, "its share of a split across spark-a, spark-b")
	})
}

func TestServeSplitComposesLeaderAndWorkers(t *testing.T) {
	ctx := context.Background()
	f := splitFixture(t, sparkLink)

	res, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "huge", Placement: backend.PlacementSplit})
	require.NoError(t, err)
	require.NotNil(t, res.Fit)
	assert.Equal(t, []string{sparkA, sparkB}, res.Fit.Nodes)

	obj := &unstructured.Unstructured{Object: f.llmisvc(ctx, "huge")}
	assert.Equal(t, backend.PlacementSplit, obj.GetAnnotations()[PlacementAnnotation])
	assert.Equal(t, "spark-a,spark-b", obj.GetAnnotations()[NodesAnnotation])

	par, _, _ := unstructured.NestedMap(obj.Object, "spec", "parallelism")
	assert.Equal(t, map[string]any{"data": int64(2), "dataLocal": int64(1)}, par, "the data-parallel shape sizes the LeaderWorkerSet")
	ann, _, _ := unstructured.NestedStringMap(obj.Object, "spec", "annotations")
	assert.Equal(t, "roce-a,roce-b", ann[networksAnnotation])
	assert.Equal(t, "false", ann[modelRoutingAnnotation])

	leader, _, _ := unstructured.NestedMap(obj.Object, "spec", "template")
	worker, _, _ := unstructured.NestedMap(obj.Object, "spec", "worker")
	require.NotNil(t, worker)
	assert.Equal(t, sparkA, leader["nodeSelector"].(map[string]any)[labelHostname])
	workerSelector, _ := worker["nodeSelector"].(map[string]any)
	_, workerPinned := workerSelector[labelHostname]
	assert.False(t, workerPinned, "the worker is placed by affinity, not by the leader's node")
	values, _, _ := unstructured.NestedSlice(worker, "affinity", "nodeAffinity", "requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms")
	assert.Contains(t, values[0].(map[string]any)["matchExpressions"].([]any)[0].(map[string]any)["values"], sparkB)

	for role, tpl := range map[string]map[string]any{"leader": leader, "worker": worker} {
		main := templateMain(tpl)
		cmd := main["command"].([]any)
		script := cmd[2].(string)
		assert.Contains(t, script, "--tensor-parallel-size 2 --nnodes 2", role)
		assert.Contains(t, script, `--master-addr "$MASTER_ADDR"`, role)
		assert.Equal(t, "--", cmd[3], role)
		assert.Contains(t, script, `eval "set -- $*"`, "%s: a preset's shell-quoted arguments are re-split", role)
		for _, a := range main["args"].([]any) {
			assert.NotContains(t, a, flagTensorParallelSize, "%s: the preset's own degree would override the split's", role)
		}
		assert.Equal(t, "gsoci.azurecr.io/giantswarm/llm-d-cuda:v0.4.0", main["image"], "%s runs the single-node template's runtime", role)
		assert.Contains(t, main["env"], map[string]any{"name": "NCCL_IB_HCA", "value": "rocep1s0f1,roceP2p1s0f1"}, role)
		req := main["resources"].(map[string]any)["requests"].(map[string]any)
		assert.Equal(t, "1", req["rdma/rdma_shared_device_a"], role)
		assert.Equal(t, "1", req["nvidia.com/gpu"], role)
		caps := main["securityContext"].(map[string]any)["capabilities"].(map[string]any)
		assert.Equal(t, []any{"NET_BIND_SERVICE"}, caps["add"], "%s: no capability the baseline Pod Security Standard refuses", role)
	}
	assert.Contains(t, templateMain(leader)["command"].([]any)[2], "--node-rank 0")
	assert.Contains(t, templateMain(leader)["command"].([]any)[2], "--served-model-name")
	leaderArgs := templateMain(leader)["args"].([]any)
	assert.Equal(t, []any{"--served-model-name"}, leaderArgs[len(leaderArgs)-4:len(leaderArgs)-3], "the leader answers under the preset name too")
	assert.Equal(t, "huge", leaderArgs[len(leaderArgs)-1])
	assert.NotContains(t, templateMain(worker)["args"], "--served-model-name", "a headless worker serves no API")
	assert.Contains(t, templateMain(worker)["command"].([]any)[2], `--node-rank "${LWS_WORKER_INDEX}"`)
	assert.Contains(t, templateMain(worker)["command"].([]any)[2], "--headless")

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, backend.PlacementSplit, loaded[0].Placement)
	assert.Equal(t, []string{sparkA, sparkB}, loaded[0].Nodes)

	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	links := map[string]string{}
	for _, n := range nodes {
		links[n.Name] = n.FastLink
	}
	assert.Equal(t, "sparks", links[sparkA])
	assert.Equal(t, "sparks", links[sparkB])
	assert.Empty(t, links[testGPUNode])
}

// splitEnvDoc is the spec.split block of the "duo" preset: one node or two.
const splitEnvDoc = `  split:
    env:
      - name: VLLM_ENABLE_ROCE_ALLREDUCE
        value: "1"
      - name: NCCL_IB_HCA
        value: rocep1s0f1
`

var splitEnvVar = map[string]any{"name": "VLLM_ENABLE_ROCE_ALLREDUCE", "value": "1"}

func TestSplitEnvReachesOnlyASplit(t *testing.T) {
	ctx := context.Background()

	t.Run("split", func(t *testing.T) {
		f := splitFixture(t, sparkLink)
		_, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "duo", Placement: backend.PlacementSplit})
		require.NoError(t, err)
		obj := f.llmisvc(ctx, "duo")
		leader, _, _ := unstructured.NestedMap(obj, "spec", "template")
		worker, _, _ := unstructured.NestedMap(obj, "spec", "worker")
		for role, tpl := range map[string]map[string]any{"leader": leader, "worker": worker} {
			env := templateMain(tpl)["env"].([]any)
			assert.Contains(t, env, splitEnvVar, role)
			var hca []any
			for _, e := range env {
				if e.(map[string]any)["name"] == "NCCL_IB_HCA" {
					hca = append(hca, e.(map[string]any)["value"])
				}
			}
			assert.Equal(t, []any{"rocep1s0f1"}, hca, "%s: the preset's split environment replaces the fast link's entry of the same name; the API refuses two", role)
		}
	})

	t.Run("one node", func(t *testing.T) {
		f := splitFixture(t, sparkLink)
		_, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "duo", Nodes: []string{sparkA}})
		require.NoError(t, err)
		obj := f.llmisvc(ctx, "duo")
		_, split, _ := unstructured.NestedMap(obj, "spec", "worker")
		require.False(t, split)
		tpl, _, _ := unstructured.NestedMap(obj, "spec", "template")
		assert.NotContains(t, templateMain(tpl)["env"], splitEnvVar, "a single-node pod never carries the split environment")
	})
}

func TestWithoutFlag(t *testing.T) {
	assert.Equal(t, []string{"--a", "--b=2"}, withoutFlag([]string{"--a", "--tensor-parallel-size", "4", "--b=2"}, flagTensorParallelSize))
	assert.Equal(t, []string{"--a"}, withoutFlag([]string{"--tensor-parallel-size=2", "--a"}, flagTensorParallelSize))
}
