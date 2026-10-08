package kserve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/crane"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// ociImage is the storage URI of the modelcar preset: the weights are an OCI
// image containerd pulls onto the node, not a download into the cache claim.
// Its registry is a closed loopback port, so the image's size is never read
// here (TestOCIPresetIsSizedFromTheImage serves one).
const ociImage = "oci://127.0.0.1:1/models/tiny-clone:abc123"

// ociPresetDoc is a preset for presetlessRepo whose storageUri is an OCI
// model image. The hub knows the repository (a 10 GiB tree) but is never
// asked for it: the image, else the preset's 10 GiB, sizes the weights.
func ociPresetDoc() string {
	return strings.Replace(presetDoc("modelcar", presetlessRepo, 10, ""), "storageUri: hf://"+presetlessRepo, "storageUri: "+ociImage, 1)
}

// serveModelImage publishes a model image the way giantswarm/models builds
// one to a registry of the test's own and returns its oci:// storage URI: a
// small layer with the checkpoint's config.json under /models (none when
// config is nil), a layer standing for the weights, and the weights label.
func serveModelImage(t *testing.T, repoTag string, weights int64, config []byte) string {
	t.Helper()
	return serveModelImageThrough(t, func(h http.Handler) http.Handler { return h }, repoTag, weights, config)
}

// serveModelImageThrough is serveModelImage with the registry behind wrap.
func serveModelImageThrough(t *testing.T, wrap func(http.Handler) http.Handler, repoTag string, weights int64, config []byte) string {
	t.Helper()
	files := map[string][]byte{"models/tokenizer_config.json": []byte("{}")}
	if config != nil {
		files[modelImageConfigPath] = config
	}
	return serveModelImageFiles(t, wrap, repoTag, weights, files)
}

// serveModelImageFiles is serveModelImageThrough with the small layer's
// files as given (a mistral-format checkpoint's params.json).
func serveModelImageFiles(t *testing.T, wrap func(http.Handler) http.Handler, repoTag string, weights int64, files map[string][]byte) string {
	t.Helper()
	reg := httptest.NewServer(wrap(registry.New()))
	t.Cleanup(reg.Close)
	small, err := crane.Layer(files)
	require.NoError(t, err)
	weightsLayer, err := random.Layer(4096, types.OCIUncompressedLayer)
	require.NoError(t, err)
	img, err := mutate.AppendLayers(empty.Image, small, weightsLayer)
	require.NoError(t, err)
	img, err = mutate.Config(img, v1.Config{Labels: map[string]string{labelModelImageWeights: strconv.FormatInt(weights, 10)}})
	require.NoError(t, err)
	ref, err := name.ParseReference(strings.TrimPrefix(reg.URL, "http://") + "/" + repoTag)
	require.NoError(t, err)
	require.NoError(t, remote.Write(ref, img))
	return "oci://" + ref.String()
}

// hubCalls is how many requests the fake hub has answered.
func (h *fakeHub) hubCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// TestPresetStoresInCache: the storage scheme decides — a bare model
// reference and every download scheme store in the cache; an OCI model
// image does not.
func TestPresetStoresInCache(t *testing.T) {
	var bare *servingPreset
	assert.True(t, bare.storesInCache(), "a bare model reference is a Hugging Face download into the cache")
	for _, uri := range []string{"hf://" + tinyRepo, "pvc://hf-cache/tiny", "s3://bucket/tiny"} {
		p, err := parsePreset([]byte(strings.Replace(presetDoc("tiny", tinyRepo, 1, ""), "storageUri: hf://"+tinyRepo, "storageUri: "+uri, 1)), "shipped")
		require.NoError(t, err)
		assert.True(t, p.storesInCache(), uri)
	}
	implicit, err := parsePreset([]byte(strings.Replace(presetDoc("tiny", tinyRepo, 1, ""), "    storageUri: hf://"+tinyRepo+"\n", "", 1)), "shipped")
	require.NoError(t, err)
	assert.Equal(t, "hf://"+tinyRepo, implicit.Spec.Model.StorageURI)
	assert.True(t, implicit.storesInCache(), "no storageUri means the hub")
	oci, err := parsePreset([]byte(ociPresetDoc()), "shipped")
	require.NoError(t, err)
	assert.False(t, oci.storesInCache())
}

// TestOCIPresetPlacesOnAnyEligibleNode: the cache claim is pinned to n1 and
// the redirect policy mounts it into every predictor, so gpu1 is no serving
// target for an hf:// preset. An oci:// preset never touches the claim: it is
// placed on gpu1 when the request names it, on gpu1 too when it does not and
// the cache node is too small for it (no cache-node preference), judged
// not cached from the oci-image source, and load_model creates its serving
// object there. The hf:// preset keeps the pin's refusal and the cache node;
// list_nodes reports the pin and that gpu1 serves model-image presets all
// the same, the verdict's answer (giantswarm/model-manager#189). Once
// served, the fit names where the preset serves and what it holds there.
func TestOCIPresetPlacesOnAnyEligibleNode(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("modelcar", ociPresetDoc()))
	f.setDiscovery(ctx, nil, true)
	const pinReason = "cache claim hf-cache is pinned to n1"

	// Named: the oci:// preset is placed on the node the claim excludes.
	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar", Node: testGPUNode})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testGPUNode, res.Node)
	assert.Equal(t, int64(10)*gib, res.WeightsBytes, "the preset's requirements: the image's registry does not answer")
	assert.Equal(t, weightsSourcePreset, res.WeightsSource)
	assert.Zero(t, f.hub.hubCalls(), "never the hub for an oci:// preset")
	assert.False(t, res.Cached)
	assert.Equal(t, backend.CacheSourceOCIImage, res.CacheSource)

	// Unnamed: the smallest eligible node that hosts it — the cache node's
	// 64 GiB under gpu1's 128 GiB (giantswarm/model-manager#254). With the
	// cache node too small for it, gpu1: no cache-node preference for an
	// oci:// preset, while the hf:// preset keeps the cache node.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testCacheNode, res.Node, "the smallest node that hosts it")
	setBudget := func(value string) {
		t.Helper()
		n, err := f.cs.CoreV1().Nodes().Get(ctx, testCacheNode, metav1.GetOptions{})
		require.NoError(t, err)
		if value == "" {
			delete(n.Annotations, BudgetAnnotation)
		} else {
			metav1.SetMetaDataAnnotation(&n.ObjectMeta, BudgetAnnotation, value)
		}
		_, err = f.cs.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{})
		require.NoError(t, err)
		f.b.inv.invalidate()
	}
	setBudget("4")
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Equal(t, testGPUNode, res.Node, "no cache-node preference for an oci:// preset")
	assert.Equal(t, backend.CacheSourceOCIImage, res.CacheSource)
	assert.Zero(t, f.hub.hubCalls())
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.Equal(t, testCacheNode, res.Node, "an hf:// preset keeps the cache node")
	assert.NotEqual(t, backend.CacheSourceOCIImage, res.CacheSource, "the claim answers for an hf:// preset")
	setBudget("")

	// The hf:// preset keeps the pin's refusal on gpu1, on the fit check and on
	// load_model, before any object exists.
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo, Node: testGPUNode})
	require.NoError(t, err)
	assert.False(t, res.Fits)
	assert.Equal(t, "node gpu1 is not a serving target: "+pinReason, res.Reason)
	err = f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo, Node: testGPUNode})
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.ErrorContains(t, err, pinReason)
	assert.Equal(t, 0, servingObjects(t, f, ctx))

	// list_nodes: the pin is a fact about the node, and gpu1 is a serving
	// target for a model-image preset all the same, as the verdict says.
	nodes, err := f.b.ListNodes(ctx)
	require.NoError(t, err)
	for _, n := range nodes {
		assert.True(t, n.ModelImageEligible, n.Name)
		if n.Name == testGPUNode {
			assert.False(t, n.Eligible)
			assert.Equal(t, pinReason, n.EligibilityReason)
		}
	}

	// load_model places the oci:// preset on gpu1; the object carries the image.
	got, err := f.b.Serve(ctx, backend.LoadRequest{Preset: "modelcar", Node: testGPUNode})
	require.NoError(t, err)
	require.NotNil(t, got.Fit)
	assert.Equal(t, testGPUNode, got.Fit.Node)
	assert.Equal(t, backend.CacheSourceOCIImage, got.Fit.CacheSource)
	assert.Equal(t, 1, servingObjects(t, f, ctx))
	uri, _, err := unstructured.NestedString(f.llmisvc(ctx, "modelcar"), "spec", "model", "uri")
	require.NoError(t, err)
	assert.Equal(t, ociImage, uri)

	// Served: the fit names the node and what the preset holds there.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "modelcar-kserve-workload-1", Namespace: testServingNS, Labels: map[string]string{"app.kubernetes.io/part-of": "llminferenceservice", llmisvcPodLabel: "modelcar"}},
		Spec:       corev1.PodSpec{NodeName: testGPUNode},
	}
	_, err = f.cs.CoreV1().Pods(testServingNS).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar"})
	require.NoError(t, err)
	assert.Equal(t, []string{testGPUNode}, res.ServingNodes)
	assert.Contains(t, res.Reason, "modelcar already serves on gpu1, holding ")
}

// TestOCIPresetOnPoolAtZero: on a GPU pool without a node the oci:// preset
// is judged against the pool's shapes like any other, and the cache verdict
// is the oci-image one instead of asking the claim.
func TestOCIPresetOnPoolAtZero(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("modelcar", ociPresetDoc()))
	f.setPool(ctx, shapeL40S)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar"})
	require.NoError(t, err)
	assert.True(t, res.Fits, res.Reason)
	assert.Empty(t, res.Node)
	assert.Equal(t, budgetSourcePoolScaleFromZero, res.BudgetSource)
	assert.Equal(t, "g6e.xlarge", res.InstanceType)
	assert.False(t, res.Cached)
	assert.Equal(t, backend.CacheSourceOCIImage, res.CacheSource)
}

// TestPullRefusesAnOCIPreset: pull_model on an oci:// preset — named, resolved
// from its model, or given as the reference — is refused as invalid before
// the hub is asked, naming the image the platform pre-pulls, and no Job
// exists. The hf:// preset's pull keeps its verdicts: refused on the
// pinned-out node with the pin's reason, after the hub sized it.
func TestPullRefusesAnOCIPreset(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, presetConfigMap("modelcar", ociPresetDoc()))
	f.setDiscovery(ctx, nil, true)

	const refusal = "invalid request: " + presetlessRepo + " is served from an OCI model image the platform pre-pulls on its GPU nodes (preset modelcar, " + ociImage + "); nothing to download"
	for _, req := range []backend.PullRequest{
		{Ref: presetlessRepo, Preset: "modelcar"},
		{Ref: presetlessRepo},
		{Ref: "modelcar"},
		{Ref: presetlessRepo, Preset: "modelcar", Node: testGPUNode},
	} {
		err := f.b.Pull(ctx, req, nil)
		require.ErrorIs(t, err, backend.ErrInvalid, "%+v", req)
		assert.EqualError(t, err, refusal, "%+v", req)
	}
	assert.Zero(t, f.hub.hubCalls(), "refused before any hub call")
	jobList, err := f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobList.Items, "no download Job for an OCI model image")

	err = f.b.Pull(ctx, backend.PullRequest{Ref: tinyRepo, Node: testGPUNode}, nil)
	require.ErrorIs(t, err, backend.ErrUnfit)
	assert.ErrorContains(t, err, "cache claim hf-cache is pinned to n1")
	assert.Positive(t, f.hub.hubCalls(), "an hf:// preset is sized by the hub as before")
	jobList, err = f.cs.BatchV1().Jobs(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, jobList.Items)
}

// TestComposeOCIPresetCarriesTheModelcarEnvironment: a preset served from a
// model image gets the modelcar environment on its main container without
// declaring it (giantswarm/model-manager#146); a name the preset sets keeps
// the preset's value and place, and an hf:// preset gets none of it.
func TestComposeOCIPresetCarriesTheModelcarEnvironment(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	s := f.b.cfg.settings(ctx)
	schema := loadLLMISVCSchema(t)
	modelcar := []any{
		map[string]any{"name": "HOME", "value": "/tmp"},
		map[string]any{"name": "HF_HOME", "value": "/tmp/hf"},
		map[string]any{"name": "VLLM_CACHE_ROOT", "value": "/tmp/vllm-cache"},
		map[string]any{"name": "TORCHINDUCTOR_CACHE_DIR", "value": "/tmp/torchinductor"},
		map[string]any{"name": "USER", "value": "vllm"},
		map[string]any{"name": "LOGNAME", "value": "vllm"},
	}

	t.Run("an oci:// preset with no env: the modelcar environment", func(t *testing.T) {
		p, err := parsePreset([]byte(ociPresetDoc()), "shipped")
		require.NoError(t, err)
		obj := f.b.composeLLM(p, s, "", referenceShape(p))
		schema.assertValid(t, obj)
		assert.Equal(t, modelcar, mainContainer(obj)["env"])
	})

	t.Run("the preset's own value wins, in its place", func(t *testing.T) {
		doc := ociPresetDoc() + `  env:
    - {name: USER, value: runtime}
    - {name: VLLM_LOGGING_LEVEL, value: DEBUG}
`
		p, err := parsePreset([]byte(doc), "shipped")
		require.NoError(t, err)
		obj := f.b.composeLLM(p, s, "", referenceShape(p))
		schema.assertValid(t, obj)
		env := mainContainer(obj)["env"].([]any)
		assert.Equal(t, map[string]any{"name": "USER", "value": "runtime"}, env[0])
		assert.Equal(t, map[string]any{"name": "VLLM_LOGGING_LEVEL", "value": "DEBUG"}, env[1])
		names := map[string]int{}
		for _, e := range env {
			names[e.(map[string]any)["name"].(string)]++
		}
		assert.Equal(t, 1, names["USER"], "one USER: the preset's")
		assert.Len(t, env, 7, "the preset's two, then the five modelcar names it does not set")
		assert.Equal(t, map[string]any{"name": "LOGNAME", "value": "vllm"}, env[6])
	})

	t.Run("an hf:// preset: its env as written", func(t *testing.T) {
		p, err := parsePreset([]byte(presetDoc("tiny", tinyRepo, 1, "")), "shipped")
		require.NoError(t, err)
		_, hasEnv := mainContainer(f.b.composeLLM(p, s, "", referenceShape(p)))["env"]
		assert.False(t, hasEnv)
	})
}

// TestOCIPresetIsSizedFromTheImage: check_fit for a preset served from a
// model image asks its registry and never the hub (giantswarm/model-manager#189):
// the weights from the image's label, downloadBytes from its layers
// (giantswarm/model-manager#150) on a node that does not hold the image, and
// nothing to download on one whose kubelet lists it — named in
// prePulledNodes and the reason. A registry that does not answer leaves the
// download unknown and the preset's numbers standing, and an hf:// preset
// still counts the repository's files.
func TestOCIPresetIsSizedFromTheImage(t *testing.T) {
	ctx := context.Background()
	const weights = int64(9) * gib
	image := serveModelImage(t, "models/tiny-clone:abc123", weights, nil)
	ref, err := name.ParseReference(strings.TrimPrefix(image, "oci://"))
	require.NoError(t, err)
	img, err := remote.Image(ref)
	require.NoError(t, err)
	manifest, err := img.Manifest()
	require.NoError(t, err)
	var layers int64
	for _, l := range manifest.Layers {
		layers += l.Size
	}

	served := strings.Replace(ociPresetDoc(), ociImage, image, 1)
	f := newFixture(t, presetConfigMap("modelcar", served), presetConfigMap("offline", strings.Replace(ociPresetDoc(), "name: modelcar", "name: offline", 1)))
	f.setDiscovery(ctx, nil, true)
	gpu, err := f.cs.CoreV1().Nodes().Get(ctx, testGPUNode, metav1.GetOptions{})
	require.NoError(t, err)
	gpu.Status.Images = append(gpu.Status.Images, corev1.ContainerImage{Names: []string{ref.Context().Name() + "@sha256:0123", ref.String()}, SizeBytes: layers})
	_, err = f.cs.CoreV1().Nodes().UpdateStatus(ctx, gpu, metav1.UpdateOptions{})
	require.NoError(t, err)

	res, err := f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar", Node: testGPUNode})
	require.NoError(t, err)
	assert.Equal(t, weights, res.WeightsBytes, "the image's weights label")
	assert.Equal(t, weightsSourceModelImage, res.WeightsSource)
	assert.Equal(t, []string{testGPUNode}, res.PrePulledNodes)
	assert.Zero(t, res.DownloadBytes, "the image is on the node")
	assert.Contains(t, res.Reason, "served from the model image, pre-pulled on gpu1")
	assert.Contains(t, res.Reason, "the checkpoint has no config.json")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "modelcar", Node: testCacheNode})
	require.NoError(t, err)
	assert.Equal(t, layers, res.DownloadBytes, "the image's layers, pulled onto n1")
	assert.Contains(t, res.Reason, "the model image is pre-pulled on gpu1, not on the node it goes to")
	assert.NotContains(t, res.Reason, "download size is unknown")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Preset: "offline", Node: testGPUNode})
	require.NoError(t, err)
	assert.Zero(t, res.DownloadBytes, "never the hub tree")
	assert.Equal(t, weightsSourcePreset, res.WeightsSource)
	assert.Contains(t, res.Reason, "the download size is unknown: read the manifest of 127.0.0.1:1/models/tiny-clone:abc123")
	assert.Zero(t, f.hub.hubCalls(), "no hub request for an oci:// preset")

	res, err = f.b.FitCheck(ctx, backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.Positive(t, res.DownloadBytes, "an hf:// preset counts the repository's files")
}

// TestImageOnNode: a node holds a model image when its kubelet lists the
// reference as written or the same repository at the same tag or digest.
func TestImageOnNode(t *testing.T) {
	const uri = "oci://gsoci.azurecr.io/giantswarm/models/qwen3-5-4b:851bf6e806ef"
	assert.True(t, imageOnNode(uri, []string{"gsoci.azurecr.io/giantswarm/models/qwen3-5-4b@sha256:abc", "gsoci.azurecr.io/giantswarm/models/qwen3-5-4b:851bf6e806ef"}))
	assert.False(t, imageOnNode(uri, []string{"gsoci.azurecr.io/giantswarm/models/qwen3-5-4b:other"}), "another tag")
	assert.False(t, imageOnNode(uri, []string{"gsoci.azurecr.io/giantswarm/models/qwen3-5-9b:851bf6e806ef"}), "another repository")
	assert.False(t, imageOnNode(uri, nil))
}

// TestModelImageReadRetriesAHangingRegistry (giantswarm/model-manager#249):
// a registry request left hanging — the ping (/v2/) timing out —
// fails after the request timeout and is retried within the fit check's
// budget; a registry that keeps hanging is reported as not answering.
func TestModelImageReadRetriesAHangingRegistry(t *testing.T) {
	prev := registryRequestTimeout
	registryRequestTimeout = 100 * time.Millisecond
	t.Cleanup(func() { registryRequestTimeout = prev })
	var hang, pings atomic.Int32
	image := serveModelImageThrough(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/" {
				pings.Add(1)
			}
			if hang.Add(-1) >= 0 {
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
				return
			}
			h.ServeHTTP(w, r)
		})
	}, "models/tiny-clone:abc123", 9*gib, nil)
	ctx := context.Background()

	hang.Store(1)
	pings.Store(0)
	rctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	img, err := readModelImage(rctx, image)
	require.NoError(t, err, "the hanging ping is retried")
	assert.Equal(t, 9*gib, img.WeightsBytes)
	assert.Equal(t, int32(2), pings.Load(), "the ping, then its retry")

	hang.Store(1000)
	start := time.Now()
	_, err = readModelImage(rctx, image)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 2*time.Second, "bounded by the request timeout and the retries, not the budget")
	assert.Contains(t, describeRegistryFailure(err, 4*time.Second), "the registry did not answer within 4s, timed-out requests retried (")
}
