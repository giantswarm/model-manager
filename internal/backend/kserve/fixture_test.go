package kserve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/model-manager/internal/backend"
)

const (
	testPlatformNS = "agent-platform"
	testServingNS  = "model-serving"
	testCacheNode  = "n1"
	testGPUNode    = "gpu1"
	testCPUNode    = "cpu1"
	tinyRepo       = "org/tiny"
	bigRepo        = "org/big"
	repackRepo     = "org/repack"
	gatedRepo      = "org/gated"
	presetlessRepo = "other/tiny-clone"
)

// fakeHub is an httptest Hugging Face Hub with a few repositories.
type fakeHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []string
	// token, when set, is required for gatedRepo.
	token string
}

func newFakeHub(t *testing.T) *fakeHub {
	t.Helper()
	f := &fakeHub{}
	mux := http.NewServeMux()
	record := func(r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.RequestURI())
		f.mu.Unlock()
	}
	mux.HandleFunc("GET /api/models", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		q := r.URL.Query().Get("search")
		var hits []map[string]any
		for _, id := range []string{tinyRepo, bigRepo, gatedRepo, presetlessRepo} {
			if strings.Contains(id, q) {
				hits = append(hits, map[string]any{"id": id, "modelId": id, "downloads": 100, "likes": 3, "gated": id == gatedRepo, "private": false, "pipeline_tag": "text-generation", "library_name": "transformers", "tags": []string{"safetensors"}})
			}
		}
		_ = json.NewEncoder(w).Encode(hits)
	})
	// Repository metadata.
	mux.HandleFunc("GET /api/models/{owner}/{name}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		id := r.PathValue("owner") + "/" + r.PathValue("name")
		switch id {
		case tinyRepo:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "sha": "abc", "gated": false, "private": false, "siblings": []map[string]string{{"rfilename": "config.json"}, {"rfilename": "model.safetensors"}}, "safetensors": map[string]any{"total": 111968}})
		case bigRepo, repackRepo:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "sha": "def", "gated": false, "private": false, "siblings": []map[string]string{{"rfilename": "model.safetensors.index.json"}}})
		case presetlessRepo:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "sha": "ghi", "gated": false, "private": false})
		case gatedRepo:
			if f.token == "" || r.Header.Get("Authorization") != "Bearer "+f.token {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"gated"}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "gated": "auto", "private": false})
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"Repository not found"}`))
		}
	})
	// File tree.
	mux.HandleFunc("GET /api/models/{owner}/{name}/tree/{rev}", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		id := r.PathValue("owner") + "/" + r.PathValue("name")
		switch id {
		case tinyRepo:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "file", "path": "config.json", "size": 807},
				{"type": "file", "path": "model.safetensors", "size": 453864, "lfs": map[string]any{"size": 453864}},
				{"type": "file", "path": "pytorch_model.bin", "size": 3561811, "lfs": map[string]any{"size": 3561811}},
				{"type": "directory", "path": "assets"},
			})
		case bigRepo:
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "file", "path": "model.safetensors.index.json", "size": 1000},
				{"type": "file", "path": "model-00001-of-00002.safetensors", "size": 50 * gib, "lfs": map[string]any{"size": 50 * gib}},
				{"type": "file", "path": "model-00002-of-00002.safetensors", "size": 50 * gib, "lfs": map[string]any{"size": 50 * gib}},
			})
		case repackRepo:
			// An FP8 repack: the index keeps the BF16 total (100 GiB), the shards hold 31 GiB.
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"type": "file", "path": "model.safetensors.index.json", "size": 1000},
				{"type": "file", "path": "model-00001-of-00002.safetensors", "size": 16 * gib, "lfs": map[string]any{"size": 16 * gib}},
				{"type": "file", "path": "model-00002-of-00002.safetensors", "size": 15 * gib, "lfs": map[string]any{"size": 15 * gib}},
			})
		case gatedRepo, presetlessRepo:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "model.safetensors", "size": 10 * gib, "lfs": map[string]any{"size": 10 * gib}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// safetensors index.
	mux.HandleFunc("GET /{owner}/{name}/resolve/{rev}/model.safetensors.index.json", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if id := r.PathValue("owner") + "/" + r.PathValue("name"); id != bigRepo && id != repackRepo {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		// Both indexes declare 100 GiB over the same two shards: big's shards
		// hold that, repack's hold 31 GiB.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata":   map[string]any{"total_size": 100 * gib},
			"weight_map": map[string]string{"model.embed_tokens.weight": "model-00001-of-00002.safetensors", "model.layers.0.mlp.weight": "model-00001-of-00002.safetensors", "lm_head.weight": "model-00002-of-00002.safetensors"},
		})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// fixture wires a Backend to fake clients and a fake hub.
type fixture struct {
	t       *testing.T
	b       *Backend
	cs      *kubefake.Clientset
	dyn     *dynamicfake.FakeDynamicClient
	hub     *fakeHub
	mu      sync.Mutex
	entries map[string][]cacheEntry // node -> entries
	scans   int
	logs    map[string]string // pod name -> logs
}

func presetDoc(name, model string, weightsGiB float64, extra string) string {
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ServingPreset
metadata:
  name: %s
spec:
  displayName: %s display
  model:
    id: %s
    storageUri: hf://%s
    format: vLLM
    contextLength: 4096
    capabilities: [chat, tools]
  args:
    - --max-model-len=4096
  resources:
    gpus: 1
    requests: {cpu: "2", memory: 8Gi}
    limits: {memory: 16Gi}
  requirements:
    weightsGiB: %v
    overheadGiB: 1
%s`, name, name, model, model, weightsGiB, extra)
}

func presetConfigMap(name, doc string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "agent-platform-serving-preset-" + name,
			Namespace: testPlatformNS,
			Labels:    map[string]string{"agent-platform.giantswarm.io/serving-preset": "true", PresetLabel: name, PresetSourceLabel: "shipped"},
		},
		Data: map[string]string{presetConfigKey: doc},
	}
}

func discoveryConfigMap() *corev1.ConfigMap {
	return discoveryConfigMapWith(discoveryOpts{})
}

// discoveryConfigMapWith is the discovery ConfigMap rendering o — for a test
// that creates the document after the fixture, the way the serving slice
// publishes it while installing.
func discoveryConfigMapWith(o discoveryOpts) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultDiscoveryConfigMap, Namespace: testPlatformNS},
		Data:       map[string]string{discoveryConfigKey: discoveryDocYAML(o)},
	}
}

// discoveryOpts are the ModelServingConfig fields the tests vary: the serving
// node selector, the cache redirect-policy flag (whether the Kyverno policies
// mount the cache claim into every predictor) and the RuntimeClass.
type discoveryOpts struct {
	nodeSelector     map[string]string
	redirectPolicy   bool
	runtimeClassName string
	// gpuPool is the pool taint and label block (spec.gpuPool); absent when nil.
	gpuPool *backend.GPUPool
	// gateway is the models Gateway's origin (spec.gateway.endpoint, enabled
	// when set); the block says enabled: false when empty.
	gateway string
	// cacheDisabled renders spec.cache.enabled: false — the serving layer
	// without a cache claim.
	cacheDisabled bool
}

// discoveryDocYAML renders the ModelServingConfig document.
func discoveryDocYAML(o discoveryOpts) string {
	selector := "  nodeSelector: {}\n"
	if len(o.nodeSelector) > 0 {
		selector = "  nodeSelector:\n"
		for k, v := range o.nodeSelector {
			selector += fmt.Sprintf("    %s: %s\n", k, v)
		}
	}
	if p := o.gpuPool; p != nil {
		selector += "  gpuPool:\n"
		if p.Taint != nil {
			selector += fmt.Sprintf("    taint: {key: %q, value: %q, effect: %q}\n", p.Taint.Key, p.Taint.Value, p.Taint.Effect)
		}
		if len(p.NodeSelector) > 0 {
			selector += "    nodeSelector:\n"
			for k, v := range p.NodeSelector {
				selector += fmt.Sprintf("      %s: %s\n", k, v)
			}
		}
		if len(p.Instances) > 0 {
			selector += "    instances:\n"
			for _, s := range p.Instances {
				selector += fmt.Sprintf("      - {instanceType: %q, size: %q, vcpu: %d, memoryGiB: %d, gpus: %d, gpuMemoryGiB: %d, usableVcpu: %v, usableMemoryGiB: %v}\n",
					s.InstanceType, s.Size, s.VCPU, s.MemoryGiB, s.GPUs, s.GPUMemoryGiB, s.UsableVCPU, s.UsableMemoryGiB)
			}
		}
	}
	gateway := "  gateway:\n    enabled: false\n"
	if o.gateway != "" {
		gateway = fmt.Sprintf("  gateway:\n    enabled: true\n    name: models\n    namespace: agent-platform\n    endpoint: %s\n    pathConvention: /<namespace>/<model>/v1\n", o.gateway)
	}
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelServingConfig
spec:
  namespace: model-serving
  runtime: kserve-vllm
  gpuResourceName: nvidia.com/gpu
  runtimeClassName: %q
%s  deploymentStrategyType: Recreate
  timeoutSeconds: 1800
  cache:
    enabled: %t
    claimName: hf-cache
    mountPath: /mnt/models
    redirectPolicy: %t
%s  presets:
    namespace: agent-platform
    labelSelector: agent-platform.giantswarm.io/serving-preset=true
    names: [tiny, big]
`, o.runtimeClassName, selector, !o.cacheDisabled, o.redirectPolicy, gateway)
}

// setDiscovery rewrites the discovery ConfigMap and drops the cached settings
// so the next call sees the change.
func (f *fixture) setDiscovery(ctx context.Context, nodeSelector map[string]string, redirectPolicy bool) {
	f.t.Helper()
	f.setDiscoveryOpts(ctx, discoveryOpts{nodeSelector: nodeSelector, redirectPolicy: redirectPolicy})
}

func (f *fixture) setDiscoveryOpts(ctx context.Context, o discoveryOpts) {
	f.t.Helper()
	f.renderDiscovery(ctx, o)
	f.resetSettings()
}

// renderDiscovery rewrites the discovery document and leaves the cached
// settings as they are — the serving slice's connectivity child rendering it
// again while model-manager holds settings resolved before.
func (f *fixture) renderDiscovery(ctx context.Context, o discoveryOpts) {
	f.t.Helper()
	cm, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Get(ctx, DefaultDiscoveryConfigMap, metav1.GetOptions{})
	require.NoError(f.t, err)
	cm.Data[discoveryConfigKey] = discoveryDocYAML(o)
	_, err = f.cs.CoreV1().ConfigMaps(testPlatformNS).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(f.t, err)
}

// serveLLMAPI makes the fake API server serve the LLMInferenceService API,
// the way a cluster with the llm-d CRDs does; the fixture starts this way.
func (f *fixture) serveLLMAPI() {
	f.t.Helper()
	f.cs.Discovery().(*discoveryfake.FakeDiscovery).Resources = []*metav1.APIResourceList{{
		GroupVersion: llmisvcGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{
			{Name: llmisvcGVR.Resource, Kind: kindLLMInferenceService, Namespaced: true},
			{Name: llmisvcConfigGVR.Resource, Kind: "LLMInferenceServiceConfig", Namespaced: true},
		},
	}}
	f.resetSettings()
}

// dropLLMAPI makes the fake API server serve no serving.kserve.io API at all
// — a cluster without the llm-d CRDs.
func (f *fixture) dropLLMAPI() {
	f.t.Helper()
	f.cs.Discovery().(*discoveryfake.FakeDiscovery).Resources = nil
	f.resetSettings()
}

// testControlPlaneNS is where the fixture's llm-d control plane keeps its
// well-known configs (the platform release namespace on a real cluster).
const testControlPlaneNS = "kserve"

// wellKnownConfig is the LLMInferenceServiceConfig the llm-d controller
// composes every workload from, as the kserve-runtime-configs component
// publishes it — its presence is how the driver tells a controller from bare
// CRDs.
func wellKnownConfig() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": llmisvcConfigGVR.GroupVersion().String(),
		"kind":       "LLMInferenceServiceConfig",
		"metadata":   map[string]any{"name": wellKnownTemplateConfig, "namespace": testControlPlaneNS},
		"spec":       map[string]any{"template": map[string]any{"containers": []any{map[string]any{"name": llmisvcMainContainer, "image": "gsoci.azurecr.io/giantswarm/llm-d-cuda:v0.4.0"}}}},
	}}
}

// dropControlPlane removes the well-known config: the llm-d CRDs stand alone,
// the way an installation with kserve-llmisvc-crd on and the controller off
// looks (giantswarm/model-manager#129).
func (f *fixture) dropControlPlane(ctx context.Context) {
	f.t.Helper()
	err := f.dyn.Resource(llmisvcConfigGVR).Namespace(testControlPlaneNS).Delete(ctx, wellKnownTemplateConfig, metav1.DeleteOptions{})
	require.NoError(f.t, err)
	f.resetSettings()
}

// installControlPlane puts the well-known config back.
func (f *fixture) installControlPlane(ctx context.Context) {
	f.t.Helper()
	_, err := f.dyn.Resource(llmisvcConfigGVR).Namespace(testControlPlaneNS).Create(ctx, wellKnownConfig(), metav1.CreateOptions{})
	require.NoError(f.t, err)
	f.resetSettings()
}

// resetSettings drops the cached settings so the next call resolves again.
func (f *fixture) resetSettings() {
	f.b.cfg.mu.Lock()
	f.b.cfg.cached = nil
	f.b.cfg.mu.Unlock()
}

// expireSettings ends the cached settings' TTL so the next call refreshes
// them — and keeps them when the refresh fails.
func (f *fixture) expireSettings() {
	f.b.cfg.mu.Lock()
	f.b.cfg.fetchedAt = time.Time{}
	f.b.cfg.mu.Unlock()
}

func node(name string, memory string, labels map[string]string) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{labelHostname: name}},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(memory), corev1.ResourceCPU: resource.MustParse("8")},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo:    corev1.NodeSystemInfo{Architecture: "amd64"},
		},
	}
	for k, v := range labels {
		n.Labels[k] = v
	}
	return n
}

// withGPUs advertises count GPUs as the default GPU resource (capacity and
// allocatable), the way a device plugin does — no feature-discovery labels,
// like a unified-memory node without the GPU operator's labeller.
func withGPUs(n *corev1.Node, count int64) *corev1.Node {
	q := *resource.NewQuantity(count, resource.DecimalSI)
	if n.Status.Capacity == nil {
		n.Status.Capacity = corev1.ResourceList{}
	}
	n.Status.Capacity[corev1.ResourceName(DefaultGPUResourceName)] = q
	n.Status.Allocatable[corev1.ResourceName(DefaultGPUResourceName)] = q
	return n
}

// notReady flips the node's Ready condition to False.
func notReady(n *corev1.Node) *corev1.Node {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			n.Status.Conditions[i].Status = corev1.ConditionFalse
		}
	}
	return n
}

func newFixture(t *testing.T, objs ...runtime.Object) *fixture {
	t.Helper()
	hub := newFakeHub(t)
	base := []runtime.Object{
		discoveryConfigMap(),
		presetConfigMap("tiny", presetDoc("tiny", tinyRepo, 0.001, "")),
		presetConfigMap("big", presetDoc("big", bigRepo, 100, "  chatTemplate:\n    configMap: agent-platform-chat-template-big\n    key: chat-template.jinja\n    mountPath: /mnt/chat-template\n  scheduling:\n    nodeSelector:\n      accelerator: gpu\n  template:\n    terminationGracePeriodSeconds: 30\n")),
		// The cache node advertises its GPU as a resource only (a unified-memory
		// node without feature-discovery labels); the GPU node is known from
		// its labels alone; the CPU node must never show up as capacity.
		withGPUs(node(testCacheNode, "64Gi", map[string]string{"accelerator": "gpu"}), 1),
		node(testGPUNode, "256Gi", map[string]string{"accelerator": "gpu", labelGPUCount: "1", labelGPUMemory: "131072", labelGPUProduct: "GB10"}),
		node(testCPUNode, "32Gi", nil),
		&corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultCacheClaim, Namespace: testServingNS},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-cache"},
		},
		&corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "pv-cache"},
			Spec: corev1.PersistentVolumeSpec{NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{Key: labelHostname, Operator: corev1.NodeSelectorOpIn, Values: []string{testCacheNode}}},
			}}}}},
		},
	}
	cs := kubefake.NewSimpleClientset(append(base, objs...)...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), llmisvcListKinds(), wellKnownConfig())
	b, err := New(backend.KServeOptions{
		Dynamic:            dyn,
		Clientset:          cs,
		DiscoveryNamespace: testPlatformNS,
		HFEndpoint:         hub.srv.URL,
		HFTokenSecret:      "hf-token",
		PollInterval:       10 * time.Millisecond,
		ReadyTimeout:       2 * time.Second,
		InventoryTimeout:   time.Second,
	})
	require.NoError(t, err)
	b.log = slog.New(slog.DiscardHandler)
	b.cfg.log = b.log
	f := &fixture{t: t, b: b, cs: cs, dyn: dyn, hub: hub, entries: map[string][]cacheEntry{}, logs: map[string]string{}}
	f.serveLLMAPI()
	b.scan = func(_ context.Context, node string) ([]cacheEntry, string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.scans++
		ranOn := node
		if ranOn == "" {
			ranOn = testCacheNode
		}
		out := make([]cacheEntry, 0)
		for _, e := range f.entries[ranOn] {
			e.Node = ranOn
			out = append(out, e)
		}
		return out, ranOn, nil
	}
	b.logs = func(_ context.Context, _, name string, opts corev1.PodLogOptions) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.logs[logKey(name, opts.Previous)], nil
	}
	return f
}

// logKey is where the fixture keeps a pod's log: the running instance's
// under the pod's name, the previous instance's under a suffix.
func logKey(pod string, previous bool) string {
	if previous {
		return pod + "/previous"
	}
	return pod
}

func (f *fixture) setEntries(node string, entries ...cacheEntry) {
	f.mu.Lock()
	f.entries[node] = entries
	f.mu.Unlock()
	f.b.inv.invalidate()
}

func (f *fixture) setLogs(pod, logs string) {
	f.mu.Lock()
	f.logs[pod] = logs
	f.mu.Unlock()
}

// setPreviousLogs is the log of a pod's container instance that died — what
// `previous=true` reads.
func (f *fixture) setPreviousLogs(pod, logs string) {
	f.mu.Lock()
	f.logs[logKey(pod, true)] = logs
	f.mu.Unlock()
}

// completePods gives every cache Job of the namespace its pod and marks every
// pod Succeeded on the cache node as soon as it appears (the fake API server
// runs no controller).
func (f *fixture) completePods(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
			jobs, err := f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for i := range jobs.Items {
				j := &jobs.Items[i]
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: j.Name + "-abcde", Namespace: testServingNS, Labels: map[string]string{jobPodLabel: j.Name}},
					Spec:       corev1.PodSpec{NodeName: testCacheNode},
					Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
				}
				_, _ = f.cs.CoreV1().Pods(testServingNS).Create(ctx, pod, metav1.CreateOptions{})
			}
			pods, err := f.cs.CoreV1().Pods(testServingNS).List(ctx, metav1.ListOptions{})
			if err != nil {
				continue
			}
			for i := range pods.Items {
				p := &pods.Items[i]
				if p.Status.Phase == corev1.PodSucceeded {
					continue
				}
				p.Status.Phase = corev1.PodSucceeded
				if p.Spec.NodeName == "" {
					p.Spec.NodeName = testCacheNode
				}
				_, _ = f.cs.CoreV1().Pods(testServingNS).Update(ctx, p, metav1.UpdateOptions{})
			}
		}
	}()
}

// completeJob waits for the download Job, gives it a running pod with progress
// logs, then marks it complete.
func (f *fixture) completeJob(ctx context.Context, name string, progressLogs string) {
	f.finishJob(ctx, name, progressLogs, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
}

// failJob runs the Job's pod with the given logs, then fails the Job the way
// a podFailurePolicy rule does (reason and message on the Failed condition).
func (f *fixture) failJob(ctx context.Context, name string, logs, reason, message string) {
	f.finishJob(ctx, name, logs, batchv1.JobCondition{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message})
}

// finishJob waits for the Job to exist, runs a pod for it whose logs read as
// given, lets a couple of progress polls happen and ends the Job with cond.
func (f *fixture) finishJob(ctx context.Context, name string, logs string, cond batchv1.JobCondition) {
	go func() {
		var job *batchv1.Job
		for job == nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
			j, err := f.cs.BatchV1().Jobs(testServingNS).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				job = j
			}
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-abcde", Namespace: testServingNS, Labels: map[string]string{jobPodLabel: name}},
			Spec:       corev1.PodSpec{NodeName: testCacheNode},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		_, _ = f.cs.CoreV1().Pods(testServingNS).Create(ctx, pod, metav1.CreateOptions{})
		f.setLogs(pod.Name, logs)
		// Let a couple of progress polls happen before the end.
		time.Sleep(40 * time.Millisecond)
		job.Status.Conditions = append(job.Status.Conditions, cond)
		if cond.Type == batchv1.JobComplete {
			job.Status.Succeeded = 1
		} else {
			job.Status.Failed = 1
		}
		_, _ = f.cs.BatchV1().Jobs(testServingNS).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	}()
}

// llmisvcListKinds registers the list kinds the fake dynamic client needs for
// the serving.kserve.io resources the driver lists.
func llmisvcListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{llmisvcGVR: "LLMInferenceServiceList", llmisvcConfigGVR: "LLMInferenceServiceConfigList"}
}

// llmisvc reads the LLMInferenceService of the name from the serving namespace.
func (f *fixture) llmisvc(ctx context.Context, name string) map[string]any {
	obj, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, name, metav1.GetOptions{})
	require.NoError(f.t, err)
	return obj.Object
}

// llmisvcObject is a hand-written LLMInferenceService serving modelURI,
// carrying only the given labels.
func llmisvcObject(name, modelURI string, labels map[string]string) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": testServingNS}
	if len(labels) > 0 {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		meta["labels"] = l
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": llmisvcGVR.GroupVersion().String(),
		"kind":       kindLLMInferenceService,
		"metadata":   meta,
		"spec":       map[string]any{"model": map[string]any{"uri": modelURI}, "replicas": int64(1), "router": map[string]any{"route": map[string]any{}}},
	}}
}
