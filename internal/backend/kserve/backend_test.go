package kserve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestInfoAndCapabilities(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	assert.Equal(t, backend.NameKServe, f.b.Name())
	caps := f.b.Capabilities()
	assert.True(t, caps.Presets && caps.FitCheck && caps.NodeInventory && caps.Search && caps.Pull && caps.Load)
	assert.False(t, caps.Wire, "wire is the service's call")
	info := f.b.Info(ctx)
	assert.True(t, info.Healthy, info.Message)
	assert.Equal(t, "serving.kserve.io/v1beta1", info.Version)
	assert.Contains(t, info.Endpoint, testServingNS)
	assert.Empty(t, info.AgentEndpoint, "no single agent-facing endpoint: every served model has its own predictor URL")
	assert.Empty(t, info.Message, "discovery found")

	s := f.b.cfg.settings(ctx)
	assert.True(t, s.DiscoveryFound)
	assert.Equal(t, testServingNS, s.Namespace)
	assert.Equal(t, DefaultCacheClaim, s.CacheClaim)
	assert.Equal(t, testPlatformNS, s.PresetNamespace)
	assert.Equal(t, "Recreate", s.DeploymentStrategyType)
	assert.EqualValues(t, 1800, s.TimeoutSeconds)
}

func TestOptionsOverrideDiscovery(t *testing.T) {
	f := newFixture(t)
	f.b.opts.Namespace = "elsewhere"
	f.b.opts.CacheClaim = "other-claim"
	f.b.cfg = newConfig(f.b.opts)
	s := f.b.cfg.settings(context.Background())
	assert.Equal(t, "elsewhere", s.Namespace)
	assert.Equal(t, "other-claim", s.CacheClaim)
	assert.Equal(t, "kserve-vllm", s.Runtime, "discovery still fills the rest")
}

func TestListPresets(t *testing.T) {
	f := newFixture(t)
	presets, err := f.b.ListPresets(context.Background())
	require.NoError(t, err)
	require.Len(t, presets, 2)
	assert.Equal(t, "big", presets[0].Name)
	assert.Equal(t, bigRepo, presets[0].Model)
	assert.Equal(t, 100*gib, presets[0].WeightsBytes)
	assert.Equal(t, gib, presets[0].OverheadBytes)
	assert.Equal(t, 101*gib, presets[0].RequiredBytes)
	assert.Equal(t, "agent-platform-chat-template-big", presets[0].ChatTemplate)
	assert.Equal(t, map[string]string{"accelerator": "gpu"}, presets[0].NodeSelector)
	assert.Equal(t, "tiny", presets[1].Name)
	assert.Equal(t, "shipped", presets[1].Source)
}

func TestSearchAnnotatesPresets(t *testing.T) {
	f := newFixture(t)
	hits, err := f.b.Search(context.Background(), "tiny", 10)
	require.NoError(t, err)
	require.Len(t, hits, 2)
	assert.Equal(t, tinyRepo, hits[0].ID)
	assert.Equal(t, []string{"tiny"}, hits[0].Presets)
	assert.Empty(t, hits[1].Presets)
	_, err = f.b.Search(context.Background(), "  ", 10)
	assert.ErrorIs(t, err, backend.ErrInvalid)
}

func TestFitCheck(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Tiny model, no preset numbers needed: sizes from the tree (no index).
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, weightsSourceTree, res.WeightsSource)
	assert.EqualValues(t, 453864, res.WeightsBytes)
	assert.EqualValues(t, 807+453864+3561811, res.DownloadBytes, "a pull fetches the whole repository")
	assert.Equal(t, gib, res.OverheadBytes, "the tiny preset's overhead")
	assert.Equal(t, "tiny", res.Preset)
	assert.Equal(t, testCacheNode, res.Node, "cache node preferred")
	assert.Equal(t, budgetSourceAllocatable, res.BudgetSource)
	assert.Equal(t, 64*gib, res.BudgetBytes)
	assert.False(t, res.Cached)

	// Big model: 100 GiB (from the safetensors index) + 1 GiB > 64 GiB on the cache node.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: bigRepo})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, weightsSourceIndex, res.WeightsSource)
	assert.Equal(t, 100*gib, res.WeightsBytes)
	assert.Contains(t, res.Reason, "exceed")
	assert.Contains(t, res.Reason, testCacheNode)

	// ... but fits the GPU node when asked for it (GPU labels budget 128 GiB).
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: bigRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, budgetSourceGPULabels, res.BudgetSource)
	assert.Equal(t, 128*gib, res.BudgetBytes)

	// Unknown node / unknown model / bad reference.
	_, err = f.b.FitCheck(ctx, backend.FitRequest{Model: "nobody/nothing"})
	assert.ErrorIs(t, err, backend.ErrNotFound)
	_, err = f.b.FitCheck(ctx, backend.FitRequest{Model: "just-a-name"})
	assert.ErrorIs(t, err, backend.ErrInvalid)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: "ghost"})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Contains(t, res.Reason, `node "ghost" not found`)

	// Preset name alone resolves the model.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "tiny"})
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, res.Model)

	// Gated without a token: refused with guidance; with the token configured
	// in the Secret the hub answers.
	_, err = f.b.FitCheck(ctx, backend.FitRequest{Model: gatedRepo})
	assert.ErrorIs(t, err, backend.ErrInvalid)
	f.hub.token = "hf_secret"
	_, err = f.cs.CoreV1().Secrets(testServingNS).Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "hf-token", Namespace: testServingNS}, Data: map[string][]byte{"token": []byte("hf_secret\n")}}, metav1.CreateOptions{})
	require.NoError(t, err)
	f.b.tokenAt = time.Time{}
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: gatedRepo})
	require.NoError(t, err)
	assert.True(t, res.Gated)
	assert.True(t, res.TokenConfigured)
	assert.True(t, res.Fits, res.Reason)
}

func TestListModelsMergesCacheAndServed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.setEntries(testCacheNode,
		cacheEntry{Dir: "tiny", Bytes: 453864, Files: 2},
		cacheEntry{Dir: "hf-internal-testing-tiny-random-gpt2", Bytes: 500, Files: 3, Marker: &marker{Model: "hf-internal-testing/tiny-random-gpt2", Revision: "abc"}},
		cacheEntry{Dir: "unknown-dir", Bytes: 7, Files: 1},
	)
	// A served model whose weights are not cached.
	require.NoError(t, f.b.createISVC(ctx, f.b.compose(mustPreset(t, f, "big"), f.b.cfg.settings(ctx), "")))

	models, err := f.b.ListModels(ctx)
	require.NoError(t, err)
	byName := map[string]backend.Model{}
	for _, m := range models {
		byName[m.Name] = m
	}
	require.Len(t, models, 4, models)
	tiny := byName[tinyRepo]
	assert.Equal(t, "tiny", tiny.Path)
	assert.Equal(t, "tiny", tiny.Preset)
	assert.Equal(t, testCacheNode, tiny.Node)
	assert.True(t, *tiny.Downloaded)
	assert.Equal(t, "vLLM", tiny.Format)
	assert.Equal(t, []string{"chat", "tools"}, tiny.Capabilities)
	marked := byName["hf-internal-testing/tiny-random-gpt2"]
	assert.Equal(t, "abc", marked.Digest)
	assert.Empty(t, marked.Preset)
	assert.Equal(t, "unknown-dir", byName["unknown-dir"].Name)
	big := byName[bigRepo]
	assert.False(t, *big.Downloaded)
	assert.Equal(t, "big", big.Path)
	assert.Equal(t, 100*gib, big.SizeBytes)

	// GetModel by id, by directory, by preset name; preset-only virtual model.
	m, err := f.b.GetModel(ctx, "ORG/tiny")
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, m.Name)
	m, err = f.b.GetModel(ctx, "hf-internal-testing-tiny-random-gpt2")
	require.NoError(t, err)
	assert.Equal(t, "hf-internal-testing/tiny-random-gpt2", m.Name)
	m, err = f.b.GetModel(ctx, "big")
	require.NoError(t, err)
	assert.Equal(t, bigRepo, m.Name)
	_, err = f.b.GetModel(ctx, "nobody/nothing")
	assert.ErrorIs(t, err, backend.ErrNotFound)

	// Scans are cached within the TTL.
	before := f.scans
	_, err = f.b.ListModels(ctx)
	require.NoError(t, err)
	assert.Equal(t, before, f.scans)
}

func mustPreset(t *testing.T, f *fixture, name string) *servingPreset {
	t.Helper()
	presets, _, err := f.b.presets(context.Background())
	require.NoError(t, err)
	p := indexPresets(presets).byName[name]
	require.NotNil(t, p)
	return p
}

func TestComposeFollowsTheRecipe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	s := f.b.cfg.settings(ctx)
	obj := f.b.compose(mustPreset(t, f, "big"), s, testGPUNode)
	assert.Equal(t, "serving.kserve.io/v1beta1", obj.GetAPIVersion())
	assert.Equal(t, "InferenceService", obj.GetKind())
	assert.Equal(t, "big", obj.GetName())
	assert.Equal(t, testServingNS, obj.GetNamespace())
	assert.Equal(t, ManagedByValue, obj.GetLabels()[ManagedByLabel])
	assert.Equal(t, "big", obj.GetLabels()[PresetLabel])
	assert.Equal(t, bigRepo, obj.GetAnnotations()[ModelAnnotation])

	model, _, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "model")
	assert.Equal(t, map[string]any{"name": "vLLM"}, model["modelFormat"])
	assert.Equal(t, "kserve-vllm", model["runtime"])
	assert.Equal(t, "hf://"+bigRepo, model["storageUri"])
	assert.Equal(t, []any{"--max-model-len=4096"}, model["args"])
	req, _, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "model", "resources", "requests")
	assert.Equal(t, "1", req["nvidia.com/gpu"])
	assert.Equal(t, "2", req["cpu"])
	assert.Equal(t, "8Gi", req["memory"])
	lim, _, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "model", "resources", "limits")
	assert.Equal(t, "1", lim["nvidia.com/gpu"])
	assert.Equal(t, "16Gi", lim["memory"])
	mounts, _, _ := unstructured.NestedSlice(obj.Object, "spec", "predictor", "model", "volumeMounts")
	require.Len(t, mounts, 1)
	assert.Equal(t, "/mnt/chat-template", mounts[0].(map[string]any)["mountPath"])
	vols, _, _ := unstructured.NestedSlice(obj.Object, "spec", "predictor", "volumes")
	require.Len(t, vols, 1)
	assert.Equal(t, map[string]any{"name": "agent-platform-chat-template-big"}, vols[0].(map[string]any)["configMap"])
	ns, _, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "nodeSelector")
	assert.Equal(t, map[string]any{"accelerator": "gpu", labelHostname: testGPUNode}, ns)
	strategy, _, _ := unstructured.NestedString(obj.Object, "spec", "predictor", "deploymentStrategy", "type")
	assert.Equal(t, "Recreate", strategy)
	timeout, _, _ := unstructured.NestedInt64(obj.Object, "spec", "predictor", "timeout")
	assert.EqualValues(t, 1800, timeout)
	minReplicas, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "predictor", "minReplicas")
	assert.EqualValues(t, 1, minReplicas, "spec.predictor extras copied verbatim")
	_, hasRuntimeClass, _ := unstructured.NestedString(obj.Object, "spec", "predictor", "runtimeClassName")
	assert.False(t, hasRuntimeClass, "empty runtimeClassName is omitted")

	// Without a chat template and node pin.
	obj = f.b.compose(mustPreset(t, f, "tiny"), s, "")
	_, hasVolumes, _ := unstructured.NestedSlice(obj.Object, "spec", "predictor", "volumes")
	assert.False(t, hasVolumes)
	_, hasSelector, _ := unstructured.NestedMap(obj.Object, "spec", "predictor", "nodeSelector")
	assert.False(t, hasSelector)
}

func TestLoadUnloadLifecycle(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// Unknown on the hub -> not found; no preset -> refused; unfit preset -> refused with the reason.
	err := f.b.Load(ctx, backend.LoadRequest{Name: "nobody/nothing"})
	assert.ErrorIs(t, err, backend.ErrNotFound)
	err = f.b.Load(ctx, backend.LoadRequest{Name: presetlessRepo})
	assert.ErrorIs(t, err, backend.ErrInvalid)
	assert.ErrorContains(t, err, "no serving preset serves")
	err = f.b.Load(ctx, backend.LoadRequest{Name: bigRepo})
	assert.ErrorIs(t, err, backend.ErrUnfit)

	// Fits: the InferenceService appears; loading again is a no-op.
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	isvc := f.isvc(ctx, "tiny")
	assert.Equal(t, "hf://"+tinyRepo, mustNested(t, isvc, "spec", "predictor", "model", "storageUri"))
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}))

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, tinyRepo, loaded[0].Name)
	assert.Equal(t, "tiny", loaded[0].Resource)
	assert.Equal(t, statusPending, loaded[0].Status)
	assert.Equal(t, predictorURL("tiny", testServingNS), loaded[0].Endpoint)
	assert.EqualValues(t, 1, loaded[0].GPUs)

	// The endpoint agents get: OpenAI provider, predictor URL, served name =
	// InferenceService name, which also names the ModelConfig.
	ep := f.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, "OpenAI", ep.Provider)
	assert.Equal(t, predictorURL("tiny", testServingNS)+"/v1", ep.BaseURL)
	assert.Equal(t, "tiny", ep.Model)
	assert.Equal(t, "tiny", ep.Name)
	assert.True(t, ep.PlaceholderAPIKey)
	ep = f.b.AgentEndpoint(bigRepo)
	assert.Equal(t, predictorURL("big", testServingNS)+"/v1", ep.BaseURL, "unserved models resolve through their preset")

	// Readiness: patch the status like a controller would; WaitReady returns.
	obj, err := f.dyn.Resource(isvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	waitErr := make(chan error, 1)
	go func() { waitErr <- f.b.WaitReady(ctx, tinyRepo) }()
	time.Sleep(30 * time.Millisecond)
	obj.Object["status"] = map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		// The ingress urlScheme leaks into address.url on a TLS-terminated
		// install; the predictor Service is plain HTTP regardless.
		"address": map[string]any{"url": "https://tiny-predictor.model-serving.svc.cluster.local/"},
		"url":     "https://tiny-model-serving.example.com",
	}
	_, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	select {
	case err := <-waitErr:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("WaitReady did not return after the status became Ready")
	}
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Equal(t, statusReady, loaded[0].Status)
	assert.Equal(t, "http://tiny-predictor.model-serving.svc.cluster.local", loaded[0].Endpoint, "status.address.url wins, with the http scheme the predictor Service speaks")
	assert.Equal(t, "http://tiny-predictor.model-serving.svc.cluster.local/v1", f.b.AgentEndpoint(tinyRepo).BaseURL, "agents are wired to the http predictor, never to the ingress scheme")

	// A hand-written InferenceService of the same name blocks a load and
	// cannot be unloaded here (409); it is still listed.
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "serving.kserve.io/v1beta1", "kind": "InferenceService",
		"metadata": map[string]any{"name": "big", "namespace": testServingNS, "labels": map[string]any{ManagedByLabel: "kustomize"}},
		"spec":     map[string]any{"predictor": map[string]any{"model": map[string]any{"storageUri": "hf://" + bigRepo, "modelFormat": map[string]any{"name": "vLLM"}}}},
	}}
	_, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).Create(ctx, foreign, metav1.CreateOptions{})
	require.NoError(t, err)
	err = f.b.Load(ctx, backend.LoadRequest{Name: bigRepo, Node: testGPUNode})
	assert.ErrorIs(t, err, backend.ErrConflict)
	err = f.b.Unload(ctx, bigRepo)
	assert.ErrorIs(t, err, backend.ErrConflict)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 2, "foreign InferenceServices are listed too")
	for _, l := range loaded {
		if l.Resource == "big" {
			assert.Equal(t, "kustomize", l.ManagedBy)
		}
	}

	// One the portal created from a preset (preset label, managed-by
	// backstage) is manageable: loading it again is a no-op, unload deletes it.
	require.NoError(t, f.dyn.Resource(isvcGVR).Namespace(testServingNS).Delete(ctx, "big", metav1.DeleteOptions{}))
	portal := f.b.compose(mustPreset(t, f, "big"), f.b.cfg.settings(ctx), "")
	portal.SetLabels(map[string]string{ManagedByLabel: "backstage", PresetLabel: "big"})
	portal.SetAnnotations(nil)
	_, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).Create(ctx, portal, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: bigRepo, Node: testGPUNode}), "same model behind the same preset: no-op")
	ep = f.b.AgentEndpoint(bigRepo)
	assert.Equal(t, "big", ep.Name, "the ModelConfig is named after the InferenceService")
	assert.Equal(t, "big", ep.Model)
	require.NoError(t, f.b.Unload(ctx, bigRepo))
	_, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).Get(ctx, "big", metav1.GetOptions{})
	assert.Error(t, err, "the portal-created InferenceService was deleted")

	// Unload ours; the cache is untouched.
	require.NoError(t, f.b.Unload(ctx, tinyRepo))
	_, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	assert.Error(t, err)
	err = f.b.Unload(ctx, tinyRepo)
	assert.ErrorIs(t, err, backend.ErrNotFound)
}

func mustNested(t *testing.T, obj map[string]any, fields ...string) string {
	t.Helper()
	v, ok, err := unstructured.NestedString(obj, fields...)
	require.NoError(t, err)
	require.True(t, ok, strings.Join(fields, "."))
	return v
}

func TestPullRunsAJobWithProgress(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Unfit and gated are refused before any Job exists.
	err := f.b.Pull(ctx, backend.PullRequest{Ref: bigRepo}, nil)
	assert.ErrorIs(t, err, backend.ErrUnfit)
	err = f.b.Pull(ctx, backend.PullRequest{Ref: gatedRepo}, nil)
	assert.ErrorIs(t, err, backend.ErrInvalid)
	jobs, _ := f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	assert.Empty(t, jobs.Items)

	plan := downloadPlan{Dir: "tiny"}
	f.completeJob(ctx, plan.jobName(), "INFO start\nPROGRESS 1000\nPROGRESS 200000\n")
	var samples []backend.Progress
	err = f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, func(p backend.Progress) { samples = append(samples, p) })
	require.NoError(t, err)
	require.NotEmpty(t, samples)
	last := samples[len(samples)-1]
	assert.Equal(t, "downloaded", last.Status)
	assert.EqualValues(t, 807+453864+3561811, last.BytesTotal)
	assert.Equal(t, last.BytesTotal, last.BytesCompleted)
	var sawProgress bool
	for _, s := range samples {
		if s.Status == "downloading" && s.BytesCompleted == 200000 {
			sawProgress = true
		}
	}
	assert.True(t, sawProgress, "progress read from the pod log: %+v", samples)

	job, err := f.cs.BatchV1().Jobs(testServingNS).Get(ctx, plan.jobName(), metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, tinyRepo, job.Annotations[ModelAnnotation])
	assert.Equal(t, "tiny", job.Annotations[DirAnnotation])
	assert.Equal(t, testCacheNode, job.Annotations[NodeAnnotation], "pinned to the cache node")
	assert.Equal(t, "tiny", job.Annotations[PresetAnnotation])
	pod := job.Spec.Template.Spec
	assert.Equal(t, testCacheNode, pod.NodeName)
	require.Len(t, pod.InitContainers, 1)
	assert.Contains(t, pod.InitContainers[0].Command[2], `mkdir -p "/cache/tiny"`)
	assert.EqualValues(t, 0, *pod.InitContainers[0].SecurityContext.RunAsUser)
	require.Len(t, pod.Containers, 1)
	assert.Equal(t, DefaultDownloadImage, pod.Containers[0].Image)
	assert.EqualValues(t, downloadUID, *pod.Containers[0].SecurityContext.RunAsUser)
	env := map[string]corev1.EnvVar{}
	for _, e := range pod.Containers[0].Env {
		env[e.Name] = e
	}
	assert.Equal(t, "hf://"+tinyRepo, env["MM_SRC"].Value)
	assert.Equal(t, "/cache/tiny", env["MM_DST"].Value)
	assert.Equal(t, "hf-token", env["HF_TOKEN"].ValueFrom.SecretKeyRef.Name)
	assert.True(t, *env["HF_TOKEN"].ValueFrom.SecretKeyRef.Optional)
	assert.Equal(t, DefaultCacheClaim, pod.Volumes[0].PersistentVolumeClaim.ClaimName)

	// Already cached: no new Job, immediate success.
	f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Bytes: 453864, Files: 2, Marker: &marker{Model: tinyRepo}})
	require.NoError(t, f.cs.BatchV1().Jobs(testServingNS).Delete(ctx, plan.jobName(), metav1.DeleteOptions{}))
	samples = nil
	require.NoError(t, f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, func(p backend.Progress) { samples = append(samples, p) }))
	require.Len(t, samples, 1)
	assert.Equal(t, "already cached", samples[0].Status)
	jobs, _ = f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	assert.Empty(t, jobs.Items)
}

func TestPullCancelDeletesJobAndAdoption(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo}, nil) }()
	plan := downloadPlan{Dir: "tiny"}
	require.Eventually(t, func() bool {
		_, err := f.cs.BatchV1().Jobs(testServingNS).Get(context.Background(), plan.jobName(), metav1.GetOptions{})
		return err == nil
	}, 2*time.Second, 5*time.Millisecond)

	// The active Job is what a restarted model-manager adopts.
	pulls, err := f.b.RunningPulls(context.Background())
	require.NoError(t, err)
	require.Len(t, pulls, 1)
	assert.Equal(t, tinyRepo, pulls[0].Ref)
	assert.Equal(t, "tiny", pulls[0].Preset)
	assert.Equal(t, testCacheNode, pulls[0].Node)

	cancel()
	select {
	case err := <-done:
		assert.True(t, errors.Is(err, context.Canceled), err)
	case <-time.After(3 * time.Second):
		t.Fatal("pull did not stop on cancel")
	}
	_, err = f.cs.BatchV1().Jobs(testServingNS).Get(context.Background(), plan.jobName(), metav1.GetOptions{})
	assert.Error(t, err, "cancel deletes the Job")
}

func TestDeleteRemovesCacheDirectory(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Bytes: 453864, Files: 2})
	f.completePods(ctx)

	// Served -> conflict.
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	err := f.b.Delete(ctx, tinyRepo)
	assert.ErrorIs(t, err, backend.ErrConflict)
	require.NoError(t, f.b.Unload(ctx, tinyRepo))

	// Not cached -> not found; cached -> a cleanup pod runs on the cache node.
	err = f.b.Delete(ctx, bigRepo)
	assert.ErrorIs(t, err, backend.ErrNotFound)
	require.NoError(t, f.b.Delete(ctx, tinyRepo))
	pods, err := f.cs.CoreV1().Pods(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, pods.Items, "the cleanup pod is removed afterwards")
	assert.Equal(t, 2, f.scans, "the inventory was invalidated after the delete")
}

func TestListNodes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Bytes: 453864, Files: 2})
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	// Give the predictor a pod on the cache node so its reservation counts.
	_, err := f.cs.CoreV1().Pods(testServingNS).Create(ctx, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "tiny-predictor-x", Namespace: testServingNS, Labels: map[string]string{isvcPodLabel: "tiny"}},
		Spec:       corev1.PodSpec{NodeName: testCacheNode},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	servedList, err := f.b.listServed(ctx)
	require.NoError(t, err)
	require.Len(t, servedList, 1)
	assert.Equal(t, testCacheNode, servedList[0].Node, "predictor pod -> node")
	assert.Equal(t, "tiny", servedList[0].Preset)

	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	require.Len(t, nodes, 2)
	gpu, cache := nodes[0], nodes[1]
	assert.Equal(t, testGPUNode, gpu.Name)
	assert.Equal(t, 128*gib, gpu.BudgetBytes)
	assert.Equal(t, budgetSourceGPULabels, gpu.BudgetSource)
	assert.Equal(t, "GB10", gpu.GPUProduct)
	assert.Nil(t, gpu.Cache, "no cache on the GPU node")
	assert.Equal(t, testCacheNode, cache.Name)
	assert.Equal(t, 64*gib, cache.BudgetBytes)
	assert.Equal(t, gibToBytes(0.001)+gib, cache.ReservedBytes, "the served tiny model reserves weights + overhead")
	require.NotNil(t, cache.Cache)
	assert.Equal(t, DefaultCacheClaim, cache.Cache.Claim)
	assert.Equal(t, 1, cache.Cache.Models)
	assert.EqualValues(t, 453864, cache.Cache.BytesUsed)

	// The reservation shrinks the free budget a load fit-check uses; a load of
	// the same preset does not count against itself.
	res, err := f.b.fitCheck(ctx, backend.FitRequest{Model: bigRepo, Node: testCacheNode}, true)
	require.NoError(t, err)
	assert.Equal(t, cache.ReservedBytes, res.Result.ReservedBytes)
	assert.Equal(t, cache.FreeBytes, res.Result.FreeBytes)
	assert.False(t, res.Result.Fits)
	res, err = f.b.fitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: testCacheNode}, true)
	require.NoError(t, err)
	assert.EqualValues(t, 0, res.Result.ReservedBytes)
	assert.True(t, res.Result.Fits)
}

func TestSharedCacheAndMissingClaim(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	// Drop the node affinity: shared storage, one scan wherever it lands.
	pv, err := f.cs.CoreV1().PersistentVolumes().Get(ctx, "pv-cache", metav1.GetOptions{})
	require.NoError(t, err)
	pv.Spec.NodeAffinity = nil
	_, err = f.cs.CoreV1().PersistentVolumes().Update(ctx, pv, metav1.UpdateOptions{})
	require.NoError(t, err)
	loc, err := f.b.cacheNodes(ctx)
	require.NoError(t, err)
	assert.True(t, loc.Shared)
	f.setEntries(testCacheNode, cacheEntry{Dir: "tiny", Bytes: 1, Files: 1})
	models, err := f.b.ListModels(ctx)
	require.NoError(t, err)
	require.Len(t, models, 1)
	assert.Equal(t, testCacheNode, models[0].Node, "the scan pod's node")
	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		require.NotNil(t, n.Cache, n.Name)
		assert.True(t, n.Cache.Shared)
	}

	// No claim at all: inventory is served models only, nodes carry no cache.
	require.NoError(t, f.cs.CoreV1().PersistentVolumeClaims(testServingNS).Delete(ctx, DefaultCacheClaim, metav1.DeleteOptions{}))
	f.b.inv.invalidate()
	models, err = f.b.ListModels(ctx)
	require.NoError(t, err)
	assert.Empty(t, models)
	nodes, err = f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		assert.Nil(t, n.Cache)
	}
}

func TestNewValidatesOptions(t *testing.T) {
	_, err := New(backend.KServeOptions{})
	assert.ErrorContains(t, err, "Kubernetes access")
	f := newFixture(t)
	_, err = New(backend.KServeOptions{Dynamic: f.dyn, Clientset: f.cs, BudgetSource: "vibes"})
	assert.ErrorContains(t, err, "budget source")
}

func TestNodeBudgetAnnotation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	annotate := func(node, value string) {
		n, err := f.cs.CoreV1().Nodes().Get(ctx, node, metav1.GetOptions{})
		require.NoError(t, err)
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		if value == "" {
			delete(n.Annotations, BudgetAnnotation)
		} else {
			n.Annotations[BudgetAnnotation] = value
		}
		_, err = f.cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		require.NoError(t, err)
	}
	find := func(nodes []backend.NodeInfo, name string) backend.NodeInfo {
		for _, n := range nodes {
			if n.Name == name {
				return n
			}
		}
		t.Fatalf("node %s missing", name)
		return backend.NodeInfo{}
	}

	// The GPU node's labels say 128 GiB; the operator knows better.
	annotate(testGPUNode, "8.5")
	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	gpu := find(nodes, testGPUNode)
	assert.Equal(t, budgetSourceAnnotation, gpu.BudgetSource)
	assert.Equal(t, gibToBytes(8.5), gpu.BudgetBytes)
	assert.Equal(t, gibToBytes(8.5), gpu.FreeBytes)
	assert.Empty(t, gpu.Message)
	assert.Equal(t, budgetSourceAllocatable, find(nodes, testCacheNode).BudgetSource, "other nodes keep the configured source")

	// The fit check and therefore pull/load use the override: 100 GiB + 1 GiB
	// no longer fits the node that used to take it.
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Model: bigRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, budgetSourceAnnotation, res.BudgetSource)
	assert.Equal(t, gibToBytes(8.5), res.BudgetBytes)
	assert.Contains(t, res.Reason, budgetSourceAnnotation)
	err = f.b.Load(ctx, backend.LoadRequest{Name: bigRepo, Node: testGPUNode})
	assert.ErrorIs(t, err, backend.ErrUnfit)
	err = f.b.Pull(ctx, backend.PullRequest{Ref: bigRepo, Node: testGPUNode}, nil)
	assert.ErrorIs(t, err, backend.ErrUnfit)

	// Garbage, zero and negative values are ignored and reported.
	for _, bad := range []string{"lots", "0", "-4", ""} {
		annotate(testGPUNode, bad)
		if bad == "" {
			break
		}
		nodes, err = f.b.ListNodes(ctx)
		require.NoError(t, err)
		gpu = find(nodes, testGPUNode)
		assert.Equal(t, budgetSourceGPULabels, gpu.BudgetSource, bad)
		assert.Equal(t, 128*gib, gpu.BudgetBytes, bad)
		assert.Contains(t, gpu.Message, BudgetAnnotation, bad)
		assert.Contains(t, gpu.Message, budgetSourceGPULabels, bad)
	}
	nodes, err = f.b.ListNodes(ctx)
	require.NoError(t, err)
	assert.Empty(t, find(nodes, testGPUNode).Message, "no annotation, no message")
}
