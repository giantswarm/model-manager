package kserve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"

	"github.com/giantswarm/model-manager/internal/backend"
)

// pairLink joins the cache node, which alone mounts the node-local cache
// claim, and the GPU node.
var pairLink = backend.FastLink{Name: "pair", Nodes: []string{testCacheNode, testGPUNode}}

// gpuNodeClaim is the GPU node's own cache claim.
var gpuNodeClaim = nodeClaimName(DefaultCacheClaim, testGPUNode)

// nodeClaimsFixture is a fixture whose cache claim is pinned to the cache
// node and provisioned from a StorageClass, the redirect policy mounting the
// claim a pod names (spec.cache.nodeClaims), the two nodes joined by a fast
// link.
func nodeClaimsFixture(t *testing.T, class *string) *fixture {
	t.Helper()
	ctx := context.Background()
	f := splitFixture(t, pairLink)
	pvc, err := f.cs.CoreV1().PersistentVolumeClaims(testServingNS).Get(ctx, DefaultCacheClaim, metav1.GetOptions{})
	require.NoError(t, err)
	pvc.Spec.StorageClassName = class
	_, err = f.cs.CoreV1().PersistentVolumeClaims(testServingNS).Update(ctx, pvc, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.setDiscoveryOpts(ctx, discoveryOpts{redirectPolicy: true, nodeClaims: true})
	return f
}

func TestClaimOn(t *testing.T) {
	pinned := cacheLocation{Claim: "hf-cache", Nodes: []string{"a"}, Bound: true, NodeClaims: true, NodeCaches: map[string]string{"b": "hf-cache-b"}}
	assert.Equal(t, "hf-cache", pinned.claimOn("a"), "the claim's own node mounts it")
	assert.Equal(t, "hf-cache", pinned.claimOn(""), "without a node, the claim")
	assert.Equal(t, "hf-cache-b", pinned.claimOn("b"))
	assert.Equal(t, "hf-cache-c", pinned.claimOn("c"), "a node without its claim yet still names it")
	assert.True(t, pinned.hasCacheOn("a"))
	assert.True(t, pinned.hasCacheOn("b"))
	assert.False(t, pinned.hasCacheOn("c"))
	assert.Equal(t, []string{"a", "b"}, pinned.cacheNodeNames())

	off := pinned
	off.NodeClaims = false
	assert.Equal(t, "hf-cache", off.claimOn("b"), "without node claims every pod mounts the claim")
	shared := cacheLocation{Claim: "hf-cache", Bound: true, Shared: true, NodeClaims: true}
	assert.Equal(t, "hf-cache", shared.claimOn("b"), "a shared claim is mounted everywhere")

	assert.True(t, sameClaim(pinned, []string{"b"}))
	assert.False(t, sameClaim(pinned, []string{"a", "b"}))
	assert.False(t, sameClaim(pinned, []string{"b", "c"}))
	assert.True(t, sameClaim(off, []string{"a", "b", "c"}))

	assert.Nil(t, cacheClaimAnnotations(off, []string{"a", "b"}))
	assert.Nil(t, cacheClaimAnnotations(pinned, []string{"a"}), "a pod on the claim's node keeps the policy's default")
	assert.Equal(t, map[string]string{WorkerCacheClaimAnnotation: "hf-cache-b"}, cacheClaimAnnotations(pinned, []string{"a", "b"}))
	assert.Equal(t, map[string]string{CacheClaimAnnotation: "hf-cache-b", WorkerCacheClaimAnnotation: "hf-cache-c"}, cacheClaimAnnotations(pinned, []string{"b", "c"}))

	long := nodeClaimName("hf-cache", strings.Repeat("n", 80)+".example.internal")
	assert.LessOrEqual(t, len(long), maxNameLength)
	assert.NotEqual(t, long, nodeClaimName("hf-cache", strings.Repeat("n", 80)+".example.internalx"), "truncated names stay distinct")
}

func TestNodeClaimsNeedTheDiscoveryFlag(t *testing.T) {
	ctx := context.Background()
	f := nodeClaimsFixture(t, ptr.To("local-path"))
	f.setDiscoveryOpts(ctx, discoveryOpts{redirectPolicy: true})

	err := f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo, Node: testGPUNode}, nil)
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "cache claim hf-cache is pinned to "+testCacheNode, "an older platform mounts the claim alone")
	claims, _ := f.cs.CoreV1().PersistentVolumeClaims(testServingNS).List(ctx, metav1.ListOptions{})
	assert.Len(t, claims.Items, 1, "no node claim without the platform's word")

	f.b.opts.InventoryMode = InventoryModeDaemonSet
	f.setDiscoveryOpts(ctx, discoveryOpts{redirectPolicy: true, nodeClaims: true})
	loc, err := f.b.cacheNodes(ctx)
	require.NoError(t, err)
	assert.False(t, loc.perNode(), "the cache-agent DaemonSet mounts the claim alone")
}

func TestPullCreatesAndFillsTheNodeClaim(t *testing.T) {
	f := nodeClaimsFixture(t, ptr.To("local-path"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	plan := downloadPlan{Dir: "tiny"}
	f.completeJob(ctx, plan.jobName(), "INFO start\nPROGRESS 1000\n")
	var last backend.Progress
	require.NoError(t, f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo, Node: testGPUNode}, func(p backend.Progress) { last = p }))
	assert.Equal(t, testGPUNode, last.Node)

	pvc, err := f.cs.CoreV1().PersistentVolumeClaims(testServingNS).Get(ctx, gpuNodeClaim, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "local-path", *pvc.Spec.StorageClassName, "the cache claim's class")
	assert.Equal(t, testGPUNode, pvc.Annotations[CacheNodeAnnotation])
	assert.Equal(t, testGPUNode, pvc.Annotations[selectedNodeAnnotation], "provisioned on the node the download Job names")
	assert.Equal(t, nodeCacheComponent, pvc.Labels[ComponentLabel])

	job, err := f.cs.BatchV1().Jobs(testServingNS).Get(ctx, plan.jobName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, testGPUNode, job.Spec.Template.Spec.NodeName)
	assert.Equal(t, gpuNodeClaim, job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName, "the download fills the node's claim")

	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		switch n.Name {
		case testGPUNode:
			require.NotNil(t, n.Cache, "the node's own cache is listed")
			assert.Equal(t, gpuNodeClaim, n.Cache.Claim)
			assert.True(t, n.Eligible, n.EligibilityReason)
		case testCacheNode:
			require.NotNil(t, n.Cache)
			assert.Equal(t, DefaultCacheClaim, n.Cache.Claim)
		}
	}
}

func TestPullRefusesANodeClaimOfAStaticCache(t *testing.T) {
	ctx := context.Background()
	f := nodeClaimsFixture(t, nil)
	err := f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo, Node: testGPUNode}, nil)
	require.ErrorIs(t, err, backend.ErrInvalid)
	assert.Contains(t, err.Error(), "binds statically")
	assert.Contains(t, err.Error(), "create claim "+gpuNodeClaim)
	jobs, _ := f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	assert.Empty(t, jobs.Items, "nothing downloads without a claim on the node")
}

func TestSplitReadsEveryNodesCache(t *testing.T) {
	ctx := context.Background()
	f := nodeClaimsFixture(t, ptr.To("local-path"))
	split := backend.LoadRequest{Preset: "tiny", Placement: backend.PlacementSplit}
	weights := cacheEntry{Dir: "tiny", Bytes: 453864, Files: 2, Marker: &marker{Model: tinyRepo}}
	f.setEntries(testCacheNode, weights)

	// The GPU node has no cache yet: the fit check and the load say what to
	// pull, the node named; the recommendation still names the split.
	fit, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "tiny", Placement: backend.PlacementSplit})
	require.NoError(t, err)
	assert.False(t, fit.Fits)
	assert.Contains(t, fit.Reason, "pull_model "+tinyRepo+" with node "+testGPUNode)
	plan, err := f.b.splitCheck(ctx, backend.FitRequest{Preset: "tiny", Placement: backend.PlacementSplit}, false)
	require.NoError(t, err)
	assert.True(t, plan.Result.Fits, "judged without serving, the split hosts the model: %s", plan.Result.Reason)
	assert.Contains(t, plan.Result.Reason, "pull_model "+tinyRepo+" with node "+testGPUNode)
	_, err = f.b.Serve(ctx, split)
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "the cache of "+testGPUNode+" holds none")

	// Its claim exists, the weights are not in it: still refused.
	_, err = f.b.ensureNodeClaim(ctx, mustCacheNodes(t, f), testGPUNode)
	require.NoError(t, err)
	f.b.inv.invalidate()
	_, err = f.b.Serve(ctx, split)
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.Contains(t, err.Error(), "the cache of "+testGPUNode+" holds none")

	// Both caches hold the weights: the worker mounts the GPU node's claim.
	f.setEntries(testGPUNode, weights)
	res, err := f.b.Serve(ctx, split)
	require.NoError(t, err)
	assert.Equal(t, []string{testCacheNode, testGPUNode}, res.Fit.Nodes)
	obj := f.llmisvc(ctx, "tiny")
	ann, _, _ := unstructured.NestedStringMap(obj, "spec", "annotations")
	assert.Equal(t, gpuNodeClaim, ann[WorkerCacheClaimAnnotation])
	_, leaderNamed := ann[CacheClaimAnnotation]
	assert.False(t, leaderNamed, "the leader runs on the cache claim's node and mounts it by default")
}

func TestCopiesOnNodesOfDifferentCachesAreRefused(t *testing.T) {
	ctx := context.Background()
	f := nodeClaimsFixture(t, ptr.To("local-path"))
	fit, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "tiny", Placement: backend.PlacementCopies, Nodes: []string{testCacheNode, testGPUNode}})
	require.NoError(t, err)
	assert.False(t, fit.Fits)
	assert.Contains(t, fit.Reason, "different cache claims")

	// One copy on the GPU node mounts its own claim.
	_, err = f.b.Serve(ctx, backend.LoadRequest{Preset: "tiny", Node: testGPUNode})
	require.NoError(t, err)
	ann, _, _ := unstructured.NestedStringMap(f.llmisvc(ctx, "tiny"), "spec", "annotations")
	assert.Equal(t, gpuNodeClaim, ann[CacheClaimAnnotation])
}

func mustCacheNodes(t *testing.T, f *fixture) cacheLocation {
	t.Helper()
	loc, err := f.b.cacheNodes(context.Background())
	require.NoError(t, err)
	return loc
}
