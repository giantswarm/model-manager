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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
		case bigRepo:
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
		case gatedRepo, presetlessRepo:
			_ = json.NewEncoder(w).Encode([]map[string]any{{"type": "file", "path": "model.safetensors", "size": 10 * gib, "lfs": map[string]any{"size": 10 * gib}}})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	// safetensors index.
	mux.HandleFunc("GET /{owner}/{name}/resolve/{rev}/model.safetensors.index.json", func(w http.ResponseWriter, r *http.Request) {
		record(r)
		if r.PathValue("owner")+"/"+r.PathValue("name") != bigRepo {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"total_size": 100 * gib}})
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
  runtime: kserve-vllm
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
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultDiscoveryConfigMap, Namespace: testPlatformNS},
		Data:       map[string]string{discoveryConfigKey: discoveryDocYAML(nil, false)},
	}
}

// discoveryDocYAML renders the ModelServingConfig document with the given
// serving node selector and cache redirect-policy flag (whether the Kyverno
// policies mount the cache claim into every predictor).
func discoveryDocYAML(nodeSelector map[string]string, redirectPolicy bool) string {
	selector := "  nodeSelector: {}\n"
	if len(nodeSelector) > 0 {
		selector = "  nodeSelector:\n"
		for k, v := range nodeSelector {
			selector += fmt.Sprintf("    %s: %s\n", k, v)
		}
	}
	return fmt.Sprintf(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelServingConfig
spec:
  namespace: model-serving
  runtime: kserve-vllm
  gpuResourceName: nvidia.com/gpu
  runtimeClassName: ""
%s  deploymentStrategyType: Recreate
  timeoutSeconds: 1800
  cache:
    enabled: true
    claimName: hf-cache
    mountPath: /mnt/models
    redirectPolicy: %t
  presets:
    namespace: agent-platform
    labelSelector: agent-platform.giantswarm.io/serving-preset=true
    names: [tiny, big]
`, selector, redirectPolicy)
}

// setDiscovery rewrites the discovery ConfigMap and drops the cached settings
// so the next call sees the change.
func (f *fixture) setDiscovery(ctx context.Context, nodeSelector map[string]string, redirectPolicy bool) {
	f.t.Helper()
	cm, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Get(ctx, DefaultDiscoveryConfigMap, metav1.GetOptions{})
	require.NoError(f.t, err)
	cm.Data[discoveryConfigKey] = discoveryDocYAML(nodeSelector, redirectPolicy)
	_, err = f.cs.CoreV1().ConfigMaps(testPlatformNS).Update(ctx, cm, metav1.UpdateOptions{})
	require.NoError(f.t, err)
	f.b.cfg.mu.Lock()
	f.b.cfg.cached = nil
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
		presetConfigMap("big", presetDoc("big", bigRepo, 100, "  chatTemplate:\n    configMap: agent-platform-chat-template-big\n    key: chat-template.jinja\n    mountPath: /mnt/chat-template\n  scheduling:\n    nodeSelector:\n      accelerator: gpu\n  predictor:\n    minReplicas: 1\n")),
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
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{isvcGVR: "InferenceServiceList"})
	b, err := New(backend.KServeOptions{
		Dynamic:            dyn,
		Clientset:          cs,
		DiscoveryNamespace: testPlatformNS,
		HFEndpoint:         hub.srv.URL,
		HFTokenSecret:      "hf-token",
		PollInterval:       10 * time.Millisecond,
		ReadyTimeout:       2 * time.Second,
		InventoryTimeout:   5 * time.Second,
	})
	require.NoError(t, err)
	b.log = slog.New(slog.DiscardHandler)
	f := &fixture{t: t, b: b, cs: cs, dyn: dyn, hub: hub, entries: map[string][]cacheEntry{}, logs: map[string]string{}}
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
	b.logs = func(_ context.Context, _, name string, _ int64) (string, error) {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.logs[name], nil
	}
	return f
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

// completePods marks every pod of the namespace Succeeded on the cache node as
// soon as it appears (the fake API server runs nothing).
func (f *fixture) completePods(ctx context.Context) {
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Millisecond):
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
		f.setLogs(pod.Name, progressLogs)
		// Let a couple of progress polls happen before completion.
		time.Sleep(40 * time.Millisecond)
		job.Status.Conditions = append(job.Status.Conditions, batchv1.JobCondition{Type: batchv1.JobComplete, Status: corev1.ConditionTrue})
		job.Status.Succeeded = 1
		_, _ = f.cs.BatchV1().Jobs(testServingNS).UpdateStatus(ctx, job, metav1.UpdateOptions{})
	}()
}

func (f *fixture) isvc(ctx context.Context, name string) map[string]any {
	obj, err := f.dyn.Resource(isvcGVR).Namespace(testServingNS).Get(ctx, name, metav1.GetOptions{})
	require.NoError(f.t, err)
	return obj.Object
}
