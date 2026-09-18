package kserve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/giantswarm/model-manager/internal/backend"
)

// scanCount is how many scans the fake scanner ran.
func (f *fixture) scanCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.scans
}

// unbindClaim turns the cache claim into one before its first consumer
// (WaitForFirstConsumer: no volume yet).
func (f *fixture) unbindClaim(ctx context.Context) {
	f.t.Helper()
	pvc, err := f.cs.CoreV1().PersistentVolumeClaims(testServingNS).Get(ctx, DefaultCacheClaim, metav1.GetOptions{})
	require.NoError(f.t, err)
	pvc.Spec.VolumeName = ""
	_, err = f.cs.CoreV1().PersistentVolumeClaims(testServingNS).Update(ctx, pvc, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

// shareVolume drops the cache volume's node affinity (network or EBS storage:
// the claim is bound but mountable anywhere).
func (f *fixture) shareVolume(ctx context.Context) {
	f.t.Helper()
	pv, err := f.cs.CoreV1().PersistentVolumes().Get(ctx, "pv-cache", metav1.GetOptions{})
	require.NoError(f.t, err)
	pv.Spec.NodeAffinity = nil
	_, err = f.cs.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

// addPoolNode adds a node of the GPU pool.
func (f *fixture) addPoolNode(ctx context.Context) {
	f.t.Helper()
	n := node("pool-1", "64Gi", map[string]string{poolLabel: poolName, labelGPUCount: "1", labelGPUMemory: "24576", labelGPUProduct: "L4"})
	_, err := f.cs.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{})
	require.NoError(f.t, err)
}

// The scan decision (giantswarm/model-manager#104): a scan pod is created
// only where it cannot become the claim's first consumer or the pool's first
// pod — never on an unbound claim, on a shared claim only while a pool node
// exists, always on a claim pinned to its nodes or without a pool.
func TestScanGate(t *testing.T) {
	ctx := context.Background()
	verdict := func(t *testing.T, f *fixture) scanVerdict {
		loc, err := f.b.cacheNodes(ctx)
		require.NoError(t, err)
		return f.b.scanAllowed(ctx, loc)
	}
	t.Run("unbound claim: no scan", func(t *testing.T) {
		f := newFixture(t)
		f.unbindClaim(ctx)
		f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
		entries, err := f.b.cacheEntries(ctx)
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.Equal(t, 0, f.scanCount(), "no scan pod while the claim is unbound")
		v := verdict(t, f)
		assert.False(t, v.Allowed)
		assert.Contains(t, v.Reason, "not bound yet")
	})
	t.Run("shared claim, pool at zero: no scan", func(t *testing.T) {
		f := newFixture(t)
		f.shareVolume(ctx)
		f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
		f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
		entries, err := f.b.cacheEntries(ctx)
		require.NoError(t, err)
		assert.Empty(t, entries)
		assert.Equal(t, 0, f.scanCount(), "no scan pod while no pool node exists")
		v := verdict(t, f)
		assert.False(t, v.Allowed)
		assert.Contains(t, v.Reason, "no node of the GPU pool")
	})
	t.Run("shared claim, pool node present: scan", func(t *testing.T) {
		f := newFixture(t)
		f.shareVolume(ctx)
		f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
		f.addPoolNode(ctx)
		f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
		entries, err := f.b.cacheEntries(ctx)
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, 1, f.scanCount())
		assert.True(t, verdict(t, f).Allowed)
	})
	t.Run("shared claim, no pool: scan", func(t *testing.T) {
		f := newFixture(t)
		f.shareVolume(ctx)
		f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
		entries, err := f.b.cacheEntries(ctx)
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, 1, f.scanCount())
	})
	t.Run("pinned claim: scan on its node", func(t *testing.T) {
		f := newFixture(t)
		f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
		f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
		entries, err := f.b.cacheEntries(ctx)
		require.NoError(t, err)
		assert.Len(t, entries, 1)
		assert.Equal(t, 1, f.scanCount(), "a pinned pod launches nothing")
	})
	t.Run("nodes unreadable: no scan", func(t *testing.T) {
		f := newFixture(t)
		f.shareVolume(ctx)
		f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
		f.cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("no"))
		})
		v := verdict(t, f)
		assert.False(t, v.Allowed, "on doubt no pod")
		assert.Contains(t, v.Reason, "cannot tell")
	})
}

// A caller whose deadline leaves no room for a scan pod is answered with what
// is known and the scan runs in the background; the next caller reads its
// result (giantswarm/model-manager#104).
func TestCacheEntriesUnderDeadlineScansInBackground(t *testing.T) {
	f := newFixture(t)
	f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
	ctx, cancel := context.WithTimeout(context.Background(), f.b.opts.InventoryTimeout/2)
	defer cancel()
	start := time.Now()
	entries, err := f.b.cacheEntries(ctx)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing known yet: no scan ran inside the call")
	assert.Less(t, time.Since(start), time.Second)
	require.Eventually(t, func() bool { return f.scanCount() == 1 }, 2*time.Second, 5*time.Millisecond, "the scan ran in the background")
	entries, err = f.b.cacheEntries(ctx)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the background scan answers the next call")
	assert.Equal(t, 1, f.scanCount())
}

// While no scan can answer, the cache index stands in for it: a directory an
// InferenceService filled for the repository — in this claim and volume —
// counts as cached, anything else does not. A record bound to another
// volume, or to none (an older release's), is no verdict
// (giantswarm/model-manager#130).
func TestIsCachedFromIndexWhenNoScanCanAnswer(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.shareVolume(ctx)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()}) // pool at zero: no scan
	loc, err := f.b.cacheNodes(ctx)
	require.NoError(t, err)
	cached, source := f.b.isCached(ctx, "", "tiny", tinyRepo, loc)
	assert.False(t, cached, "index miss")
	assert.Equal(t, backend.CacheSourceUnknown, source, "no scan, no index record: unknown, not no")
	cms := f.cs.CoreV1().ConfigMaps(testServingNS)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: f.b.opts.CacheIndexConfigMap, Namespace: testServingNS},
		Data:       map[string]string{"tiny": `{"model":"` + tinyRepo + `","dir":"tiny","inferenceService":"tiny"}`},
	}
	_, err = cms.Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)
	f.b.index.set(nil)
	cached, source = f.b.isCached(ctx, "", "tiny", tinyRepo, loc)
	assert.False(t, cached, "a record bound to no cache is no verdict")
	assert.Equal(t, backend.CacheSourceUnknown, source)
	cm.Data["tiny"] = `{"model":"` + tinyRepo + `","dir":"tiny","inferenceService":"tiny","claim":"hf-cache","volume":"pv-old"}`
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.b.index.set(nil)
	cached, source = f.b.isCached(ctx, "", "tiny", tinyRepo, loc)
	assert.False(t, cached, "a record bound to another volume is no verdict")
	assert.Equal(t, backend.CacheSourceUnknown, source)
	cm.Data["tiny"] = `{"model":"` + tinyRepo + `","dir":"tiny","inferenceService":"tiny","claim":"hf-cache","volume":"pv-cache"}`
	_, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(t, err)
	f.b.index.set(nil)
	cached, source = f.b.isCached(ctx, "", "tiny", tinyRepo, loc)
	assert.True(t, cached, "index hit: bound to this claim and volume")
	assert.Equal(t, backend.CacheSourceIndex, source)
	cached, _ = f.b.isCached(ctx, "", "big", bigRepo, loc)
	assert.False(t, cached, "another directory: miss")
	assert.Equal(t, 0, f.scanCount(), "no scan pod")
}

// load_model on a fresh slice with the pool at zero: the serving object is
// composed and created within the caller's deadline, no scan pod runs inside
// the call, and the loaded models list shows the object right after
// (giantswarm/model-manager#104).
func TestLoadAnswersWithinDeadlineAtScaleFromZero(t *testing.T) {
	f := newFixture(t)
	bg := context.Background()
	f.unbindClaim(bg)
	f.setDiscoveryOpts(bg, discoveryOpts{gpuPool: poolInput()})
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	start := time.Now()
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	assert.Less(t, time.Since(start), 3*time.Second, "answered within the deadline")
	assert.Equal(t, 0, f.scanCount(), "no scan pod inside load_model")
	obj, err := f.b.findServing(bg, f.b.cfg.settings(bg), "tiny")
	require.NoError(t, err)
	require.NotNil(t, obj, "the serving object exists")
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, tinyRepo, loaded[0].Name)
	assert.Equal(t, 0, f.scanCount(), "listing the loaded models scans nothing either")
}

// The hub lookups get what the caller's deadline leaves after the reserve, at
// most opts.HFTimeout.
func TestHubContextFollowsTheCallerDeadline(t *testing.T) {
	b := &Backend{opts: backend.KServeOptions{HFTimeout: 4 * time.Second}}
	_, budget, cancel := b.hubContext(context.Background())
	cancel()
	assert.Equal(t, 4*time.Second, budget, "no deadline: the full lookup timeout")

	ctx, cancel := context.WithTimeout(context.Background(), deadlineReserve+time.Second)
	defer cancel()
	hctx, budget, hcancel := b.hubContext(ctx)
	defer hcancel()
	assert.InDelta(t, time.Second.Seconds(), budget.Seconds(), 0.1, "the deadline minus the reserve")
	dl, ok := hctx.Deadline()
	require.True(t, ok)
	assert.WithinDuration(t, time.Now().Add(time.Second), dl, 100*time.Millisecond)

	ctx, cancel = context.WithTimeout(context.Background(), deadlineReserve/2)
	defer cancel()
	_, budget, hcancel = b.hubContext(ctx)
	defer hcancel()
	assert.Equal(t, time.Duration(0), budget, "inside the reserve: nothing for the hub, the preset stands in")
}

// A scan runs as a Job that owns its pod — the fleet's prevent-bare-pods
// policy audits a pod without an owner — with no retry, a TTL, the cache
// pod's spec, and is deleted with its pod once read.
func TestScanRunsAsOwnedJob(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.completePods(ctx)
	scanOut := lineDir + "\ttiny\t100\t3\t0\t1\n" + lineEnd + "\n"
	f.b.logs = func(context.Context, string, string, corev1.PodLogOptions) (string, error) { return scanOut, nil }

	entries, ranOn, err := f.b.scanNode(ctx, testCacheNode)
	require.NoError(t, err)
	assert.Equal(t, testCacheNode, ranOn)
	require.Len(t, entries, 1)
	assert.Equal(t, "tiny", entries[0].Dir)

	var jobs []*batchv1.Job
	for _, a := range f.cs.Actions() {
		if a.GetVerb() != "create" {
			continue
		}
		switch obj := a.(k8stesting.CreateAction).GetObject().(type) {
		case *batchv1.Job:
			jobs = append(jobs, obj)
		case *corev1.Pod:
			assert.Contains(t, obj.Labels, jobPodLabel, "every pod belongs to a Job")
		}
	}
	require.Len(t, jobs, 1, "one Job per scan")
	job := jobs[0]
	assert.True(t, strings.HasPrefix(job.Name, scanPrefix), job.Name)
	assert.EqualValues(t, 0, *job.Spec.BackoffLimit, "no retry")
	assert.EqualValues(t, cacheJobTTL/time.Second, *job.Spec.TTLSecondsAfterFinished, "collected even when model-manager dies mid-scan")
	assert.Equal(t, "cache-tool", job.Labels[ComponentLabel])
	assert.Equal(t, testCacheNode, job.Spec.Template.Spec.NodeName, "pinned to the cache node")
	assert.True(t, job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly, "a scan reads")
	assertRestrictedPodSecurity(t, job.Spec.Template.Spec)
	_, err = f.cs.BatchV1().Jobs(testServingNS).Get(ctx, job.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the Job is deleted once read")
}

// unload_model with a cache scan that would outlast the caller's deadline (a
// scan pod that cannot schedule): the serving object is deleted and the call
// answers within the deadline, no scan runs inside it, and the inventory is
// rescanned in the background — the answer says so
// (giantswarm/model-manager#119).
func TestStopAnswersWithinDeadlineWithASlowScan(t *testing.T) {
	f := newFixture(t)
	bg := context.Background()
	require.NoError(t, f.b.Load(bg, backend.LoadRequest{Preset: "tiny"}))
	f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Files: 3, HasModel: true})
	before := f.scanCount()
	const deadline = 300 * time.Millisecond
	scan := f.b.scan
	f.b.scan = func(ctx context.Context, node string) ([]cacheEntry, string, error) {
		select {
		case <-time.After(3 * deadline):
		case <-ctx.Done():
			return nil, node, ctx.Err()
		}
		return scan(ctx, node)
	}

	ctx, cancel := context.WithTimeout(bg, deadline)
	defer cancel()
	start := time.Now()
	res, err := f.b.Stop(ctx, tinyRepo)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), deadline, "answered within the deadline")
	assert.Equal(t, tinyRepo, res.Model)
	assert.True(t, res.Inventory.Refreshing, "the answer says the inventory is rescanned in the background")
	assert.Empty(t, res.Inventory.Reason)
	obj, err := f.b.findServing(bg, f.b.cfg.settings(bg), "tiny")
	require.NoError(t, err)
	assert.Nil(t, obj, "the serving object is deleted")
	assert.Equal(t, before, f.scanCount(), "no scan inside the call")
	require.Eventually(t, func() bool { return f.scanCount() == before+1 }, 5*deadline, 5*time.Millisecond, "the scan ran in the background")
	require.Eventually(t, func() bool { return f.b.inv.fresh(testCacheNode, f.b.opts.InventoryTTL) != nil }, time.Second, 5*time.Millisecond, "the next read answers from the new scan")

	// Nothing serves the model any more: not found, still without a scan.
	_, err = f.b.Stop(ctx, tinyRepo)
	assert.ErrorIs(t, err, backend.ErrNotFound)
	assert.Equal(t, before+1, f.scanCount())
}

// The same unload on a GPU pool at zero: nothing may launch a scan pod, so
// the answer says the inventory is not rescanned and why; the deletion is
// done all the same, and the preset name addresses the object too.
func TestStopSaysWhyNoScanCanRun(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.shareVolume(ctx)
	f.setDiscoveryOpts(ctx, discoveryOpts{gpuPool: poolInput()})
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))
	before := f.scanCount()

	res, err := f.b.Stop(ctx, "tiny")
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, res.Model, "the answer names the repository the object served")
	assert.False(t, res.Inventory.Refreshing)
	assert.Contains(t, res.Inventory.Reason, "no node of the GPU pool")
	obj, err := f.b.findServing(ctx, f.b.cfg.settings(ctx), "tiny")
	require.NoError(t, err)
	assert.Nil(t, obj, "the serving object is deleted")
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, before, f.scanCount(), "no scan pod")
}
