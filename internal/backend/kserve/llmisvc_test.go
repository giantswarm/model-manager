package kserve

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The fixtures under testdata/llmisvc: the LLMInferenceService CRD as the
// kserve-llmisvc-crd chart ships it (gzipped; the test validates every
// composed object against its v1alpha2 schema) and the shipped
// ServingPresets as the platform chart publishes them (see the header of
// each file for the source and the refresh recipe).
const (
	llmisvcCRDFixture     = "testdata/llmisvc/crd.yaml.gz"
	shippedPresetsFixture = "testdata/llmisvc/presets.yaml"
)

// llmisvcSchema is the CRD's v1alpha2 schema as the API server applies it:
// a validator for the values and a structural schema for the shape.
type llmisvcSchema struct {
	validator  validation.SchemaValidator
	structural *structuralschema.Structural
}

func loadLLMISVCSchema(t *testing.T) llmisvcSchema {
	t.Helper()
	f, err := os.Open(llmisvcCRDFixture)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	require.NoError(t, err)
	raw, err := io.ReadAll(zr)
	require.NoError(t, err)
	var crd apiextensionsv1.CustomResourceDefinition
	require.NoError(t, yaml.Unmarshal(raw, &crd))
	require.Equal(t, ServingKindLLM, crd.Spec.Names.Kind)
	var props *apiextensionsv1.JSONSchemaProps
	for _, v := range crd.Spec.Versions {
		if v.Name == llmisvcGVR.Version {
			props = v.Schema.OpenAPIV3Schema
		}
	}
	require.NotNil(t, props, "the CRD serves %s", llmisvcGVR.Version)
	var internal apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(props, &internal, nil))
	validator, _, err := validation.NewSchemaValidator(&internal)
	require.NoError(t, err)
	structural, err := structuralschema.NewStructural(&internal)
	require.NoError(t, err)
	return llmisvcSchema{validator: validator, structural: structural}
}

// assertValid fails when the API server would reject the object or prune a
// field of it (a field the CRD does not know is a misplaced field).
func (s llmisvcSchema) assertValid(t *testing.T, obj *unstructured.Unstructured) {
	t.Helper()
	errs := validation.ValidateCustomResource(nil, obj.Object, s.validator)
	assert.Empty(t, errs, "%s %s fails the CRD schema", obj.GetKind(), obj.GetName())
	pruned := pruning.PruneWithOptions(obj.DeepCopy().Object, s.structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
	assert.Empty(t, pruned, "%s %s carries fields the CRD does not know", obj.GetKind(), obj.GetName())
}

// shippedPresets parses the published preset ConfigMaps of the fixture.
func shippedPresets(t *testing.T) []*servingPreset {
	t.Helper()
	raw, err := os.ReadFile(shippedPresetsFixture)
	require.NoError(t, err)
	var out []*servingPreset
	for _, doc := range strings.Split(string(raw), "\n---") {
		var cm corev1.ConfigMap
		if err := yaml.Unmarshal([]byte(doc), &cm); err != nil || cm.Data[presetConfigKey] == "" {
			continue
		}
		p, err := parsePreset([]byte(cm.Data[presetConfigKey]), cm.Labels[PresetSourceLabel])
		require.NoError(t, err, cm.Name)
		out = append(out, p)
	}
	require.NotEmpty(t, out)
	return out
}

func TestComposeLLMInferenceServiceForEveryShippedPreset(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	f.setDiscoveryOpts(ctx, discoveryOpts{runtimeClassName: "nvidia"})
	s := f.b.cfg.settings(ctx)
	require.Equal(t, ServingKindLLM, s.ServingKind, "auto picks the LLMInferenceService where its API is served")
	schema := loadLLMISVCSchema(t)

	withChatTemplate := 0
	for _, p := range shippedPresets(t) {
		t.Run(p.name(), func(t *testing.T) {
			obj := f.b.compose(p, s, "")
			schema.assertValid(t, obj)
			assert.Equal(t, "serving.kserve.io/v1alpha2", obj.GetAPIVersion())
			assert.Equal(t, ServingKindLLM, obj.GetKind())
			assert.Equal(t, p.name(), obj.GetName())
			assert.Equal(t, testServingNS, obj.GetNamespace())
			assert.Equal(t, ManagedByValue, obj.GetLabels()[ManagedByLabel])
			assert.Equal(t, p.name(), obj.GetLabels()[PresetLabel])
			assert.Equal(t, p.Spec.Model.ID, obj.GetAnnotations()[ModelAnnotation])

			spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
			assert.Equal(t, map[string]any{"uri": p.Spec.Model.StorageURI, "name": p.Spec.Model.ID}, spec["model"])
			assert.True(t, strings.HasPrefix(p.Spec.Model.StorageURI, "hf://"), "shipped presets serve from the Hub")
			assert.EqualValues(t, 1, spec["replicas"])
			assert.Equal(t, map[string]any{"route": map[string]any{}}, spec["router"], "the route alone: KServe routes the Gateway to the workload Service; no scheduler, no InferencePool")
			_, hasBaseRefs := spec["baseRefs"]
			assert.False(t, hasBaseRefs, "no baseRefs: KServe picks its well-known configs from the shape")

			containers, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "containers")
			require.Len(t, containers, 1, "one container: the template's main")
			main := mainContainer(obj)
			require.NotNil(t, main)
			_, hasImage := main["image"]
			assert.False(t, hasImage, "no image: the well-known template's llm-d-cuda image runs")
			assert.Equal(t, toAnySlice(p.Spec.Args), main["args"])
			if len(p.Spec.Env) > 0 {
				assert.Equal(t, mapsToAny(p.Spec.Env), main["env"])
			} else {
				_, hasEnv := main["env"]
				assert.False(t, hasEnv)
			}
			requests, _, _ := unstructured.NestedMap(main, "resources", "requests")
			limits, _, _ := unstructured.NestedMap(main, "resources", "limits")
			assert.Equal(t, "1", requests[DefaultGPUResourceName])
			assert.Equal(t, "1", limits[DefaultGPUResourceName])
			for k, v := range p.Spec.Resources.Requests {
				assert.Equal(t, fmt.Sprint(v), requests[k], "requests.%s", k)
			}
			for k, v := range p.Spec.Resources.Limits {
				assert.Equal(t, fmt.Sprint(v), limits[k], "limits.%s", k)
			}

			runtimeClass, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "runtimeClassName")
			assert.Equal(t, "nvidia", runtimeClass, "the discovery's runtimeClassName")
			_, hasSelector, _ := unstructured.NestedMap(obj.Object, "spec", "template", "nodeSelector")
			assert.False(t, hasSelector, "shipped presets pin no node")
			_, hasTolerations, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "tolerations")
			assert.False(t, hasTolerations)

			mounts, _, _ := unstructured.NestedSlice(main, "volumeMounts")
			volumes, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "volumes")
			if ct := p.Spec.ChatTemplate; ct != nil {
				withChatTemplate++
				require.Len(t, mounts, 1)
				require.Len(t, volumes, 1)
				assert.Equal(t, map[string]any{"name": "chat-template", "mountPath": ct.MountPath, "readOnly": true}, mounts[0])
				assert.Equal(t, map[string]any{"name": "chat-template", "configMap": map[string]any{"name": ct.ConfigMap}}, volumes[0])
				assert.Contains(t, p.Spec.Args, "--chat-template="+ct.MountPath+"/"+ct.Key, "the chart appended the flag the mount serves")
			} else {
				assert.Empty(t, mounts)
				assert.Empty(t, volumes)
			}
		})
	}
	assert.Positive(t, withChatTemplate, "a shipped preset with a chat template is covered")

	// An empty runtimeClassName sets nothing.
	f.setDiscoveryOpts(ctx, discoveryOpts{})
	obj := f.b.compose(shippedPresets(t)[0], f.b.cfg.settings(ctx), "")
	schema.assertValid(t, obj)
	_, hasRuntimeClass, _ := unstructured.NestedString(obj.Object, "spec", "template", "runtimeClassName")
	assert.False(t, hasRuntimeClass, "empty runtimeClassName is omitted")
}

func TestComposeLLMInferenceServicePresetOverrides(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	f.setDiscoveryOpts(ctx, discoveryOpts{nodeSelector: map[string]string{"pool": "gpu"}, runtimeClassName: "nvidia"})
	custom := presetDoc("custom", "org/custom", 1, `  env:
    - {name: VLLM_LOGGING_LEVEL, value: DEBUG}
  scheduling:
    nodeSelector: {accelerator: gpu}
    tolerations:
      - {key: nvidia.com/gpu, operator: Exists, effect: NoSchedule}
  template:
    terminationGracePeriodSeconds: 30
    containers:
      - name: main
        image: gsoci.azurecr.io/giantswarm/llm-d-cuda:custom
  baseRefs:
    - name: custom-config
`)
	_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, presetConfigMap("custom", custom), metav1.CreateOptions{})
	require.NoError(t, err)
	s := f.b.cfg.settings(ctx)
	obj := f.b.compose(mustPreset(t, f, "custom"), s, testGPUNode)
	loadLLMISVCSchema(t).assertValid(t, obj)

	main := mainContainer(obj)
	require.NotNil(t, main)
	assert.Equal(t, "gsoci.azurecr.io/giantswarm/llm-d-cuda:custom", main["image"], "the preset's image override")
	assert.Equal(t, []any{"--max-model-len=4096"}, main["args"], "merged by name: the args stay")
	assert.Equal(t, []any{map[string]any{"name": "VLLM_LOGGING_LEVEL", "value": "DEBUG"}}, main["env"])
	grace, _, _ := unstructured.NestedFieldNoCopy(obj.Object, "spec", "template", "terminationGracePeriodSeconds")
	assert.EqualValues(t, 30, grace, "template extras copied verbatim")
	selector, _, _ := unstructured.NestedMap(obj.Object, "spec", "template", "nodeSelector")
	assert.Equal(t, map[string]any{"pool": "gpu", "accelerator": "gpu", labelHostname: testGPUNode}, selector, "discovery selector, preset selector, node pin")
	tolerations, _, _ := unstructured.NestedSlice(obj.Object, "spec", "template", "tolerations")
	assert.Equal(t, []any{map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"}}, tolerations)
	baseRefs, _, _ := unstructured.NestedSlice(obj.Object, "spec", "baseRefs")
	assert.Equal(t, []any{map[string]any{"name": "custom-config"}}, baseRefs, "a preset naming a custom config is the only baseRefs case")
	runtimeClass, _, _ := unstructured.NestedString(obj.Object, "spec", "template", "runtimeClassName")
	assert.Equal(t, "nvidia", runtimeClass)
}

// The llm-d endpoint picker is opt-in: the backend's router option composes
// router.scheduler for every preset, a preset's spec.router.scheduler decides
// for itself either way. Its InferencePool needs the Inference Extension on
// the gateway, which the default shape does not.
func TestComposeLLMInferenceServiceRouterScheduler(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	for name, extra := range map[string]string{
		"picker":  "  router:\n    scheduler: true\n",
		"route":   "  router:\n    scheduler: false\n",
		"default": "",
	} {
		_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, presetConfigMap(name, presetDoc(name, "org/"+name, 1, extra)), metav1.CreateOptions{})
		require.NoError(t, err)
	}
	schema := loadLLMISVCSchema(t)
	router := func(t *testing.T, preset string) map[string]any {
		t.Helper()
		obj := f.b.compose(mustPreset(t, f, preset), f.b.cfg.settings(ctx), "")
		schema.assertValid(t, obj)
		r, _, _ := unstructured.NestedMap(obj.Object, "spec", "router")
		return r
	}
	withPicker := map[string]any{"route": map[string]any{}, "scheduler": map[string]any{}}
	routeOnly := map[string]any{"route": map[string]any{}}

	assert.Equal(t, withPicker, router(t, "picker"), "a preset switches the scheduler on")
	assert.Equal(t, routeOnly, router(t, "route"))
	assert.Equal(t, routeOnly, router(t, "default"), "off unless asked")

	// The backend option (a document's spec.kserve.router.scheduler) is the
	// default for every preset that does not decide for itself.
	f.b.cfg.opts.Router.Scheduler = true
	f.resetSettings()
	assert.Equal(t, withPicker, router(t, "default"), "the backend's default")
	assert.Equal(t, routeOnly, router(t, "route"), "a preset switches the scheduler off")
	assert.Equal(t, withPicker, router(t, "picker"))
}

func TestLoadUnloadLLMInferenceService(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()

	info := f.b.Info(ctx)
	assert.True(t, info.Healthy)
	assert.Equal(t, "serving.kserve.io/v1alpha2", info.Version)

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	llmisvcs, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, llmisvcs.Items, 1, "exactly one LLMInferenceService")
	isvcs, err := f.dyn.Resource(isvcGVR).Namespace(testServingNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, isvcs.Items, "no classic InferenceService")
	assert.Equal(t, "hf://"+tinyRepo, mustNested(t, llmisvcs.Items[0].Object, "spec", "model", "uri"))
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Preset: "tiny"}), "loading again is a no-op")

	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, tinyRepo, loaded[0].Name)
	assert.Equal(t, "tiny", loaded[0].Resource)
	assert.Equal(t, ServingKindLLM, loaded[0].Kind, "a loaded model names its kind")
	assert.Equal(t, statusPending, loaded[0].Status)
	assert.Equal(t, workloadURL("tiny", testServingNS), loaded[0].Endpoint, "the workload Service until KServe publishes an address")
	assert.EqualValues(t, 1, loaded[0].GPUs)
	ep := f.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, workloadURL("tiny", testServingNS)+"/v1", ep.BaseURL)
	assert.Equal(t, tinyRepo, ep.Model, "the well-known template serves under spec.model.name")
	assert.Equal(t, "tiny", ep.Name, "the ModelConfig is named after the object")
	assert.True(t, ep.PlaceholderAPIKey, "the in-cluster workload Service is keyless vLLM: kagent's placeholder key")
	assert.False(t, ep.APIKeyPassthrough)
	ep = f.b.AgentEndpoint(bigRepo)
	assert.Equal(t, workloadURL("big", testServingNS)+"/v1", ep.BaseURL, "unserved models resolve through their preset, in the configured kind")
	assert.Equal(t, bigRepo, ep.Model)

	// KServe publishes the routed address; the wiring follows it.
	obj, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	obj.Object["status"] = map[string]any{
		"conditions": []any{map[string]any{"type": "Ready", "status": "True"}},
		"addresses":  []any{map[string]any{"url": "https://models.example.com/model-serving/tiny"}},
	}
	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Equal(t, statusReady, loaded[0].Status)
	assert.Equal(t, "https://models.example.com/model-serving/tiny", loaded[0].Endpoint)
	ep = f.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, "https://models.example.com/model-serving/tiny/v1", ep.BaseURL, "the wiring follows the route")
	assert.True(t, ep.APIKeyPassthrough, "a model routed on the models Gateway is reached with the caller's own token")
	assert.False(t, ep.PlaceholderAPIKey, "no placeholder key: the Gateway admits a person's token only")

	// A classic InferenceService of the same name, whoever made it, blocks a load.
	foreign := isvcObject("big", "hf://"+bigRepo, nil)
	_, err = f.dyn.Resource(isvcGVR).Namespace(testServingNS).Create(ctx, foreign, metav1.CreateOptions{})
	require.NoError(t, err)
	err = f.b.Load(ctx, backend.LoadRequest{Preset: "big"})
	assert.ErrorIs(t, err, backend.ErrConflict)
	assert.ErrorContains(t, err, "InferenceService model-serving/big exists")
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 2, "both kinds are listed")

	require.NoError(t, f.b.Unload(ctx, tinyRepo))
	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	assert.Error(t, err, "the LLMInferenceService is gone")
	err = f.b.Unload(ctx, bigRepo)
	assert.ErrorIs(t, err, backend.ErrConflict, "a hand-written InferenceService is not model-manager's to delete")
}

// With the models Gateway named in discovery, an LLMInferenceService's address
// is known the moment it is composed — its route on the Gateway — so the
// ModelConfig a load wires points where the model will answer and forwards
// the caller's token, before KServe has published anything
// (giantswarm/model-manager#115).
func TestLLMInferenceServiceIsRoutedOnTheDiscoveryGatewayBeforeKServePublishes(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serveLLMAPI()
	f.setDiscoveryOpts(ctx, discoveryOpts{gateway: "https://models.example.com/"})

	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, statusPending, loaded[0].Status, "nothing published yet")
	assert.Equal(t, "https://models.example.com/model-serving/tiny", loaded[0].Endpoint, "the route KServe will render, from the discovery Gateway (trailing slash dropped)")

	ep := f.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, "https://models.example.com/model-serving/tiny/v1", ep.BaseURL)
	assert.Equal(t, tinyRepo, ep.Model)
	assert.Equal(t, "tiny", ep.Name)
	assert.True(t, ep.APIKeyPassthrough, "routed on the Gateway: the caller's token, before the model is ready")
	assert.False(t, ep.PlaceholderAPIKey)

	// A model not served yet composes the same way.
	ep = f.b.AgentEndpoint(bigRepo)
	assert.Equal(t, "https://models.example.com/model-serving/big/v1", ep.BaseURL)
	assert.True(t, ep.APIKeyPassthrough)

	// Once KServe publishes an address, that one wins.
	obj, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	obj.Object["status"] = map[string]any{"addresses": []any{map[string]any{"url": "https://models.example.com/model-serving/tiny-renamed"}}}
	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Equal(t, "https://models.example.com/model-serving/tiny-renamed", loaded[0].Endpoint)
	assert.Equal(t, "https://models.example.com/model-serving/tiny-renamed/v1", f.b.AgentEndpoint(tinyRepo).BaseURL)

	// KServe publishing the Gateway's in-cluster address — the Gateway's
	// Service name, what a Gateway without a load balancer address gets —
	// keeps the route under the discovery Gateway's origin: the Gateway
	// terminates TLS and demands the person's token there too, and a
	// cluster-local https address would otherwise be taken for the keyless
	// workload Service and rewritten to plain http.
	obj, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	// KServe writes the route into status.url and its addresses alike; the
	// controller's own root address comes first in the list.
	obj.Object["status"] = map[string]any{
		"url":       "https://models.agent-platform.svc.cluster.local/model-serving/tiny",
		"addresses": []any{map[string]any{"url": "https://models.agent-platform.svc.cluster.local/"}, map[string]any{"url": "https://models.agent-platform.svc.cluster.local/model-serving/tiny"}},
	}
	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	loaded, err = f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Equal(t, "https://models.example.com/model-serving/tiny", loaded[0].Endpoint, "the Gateway's origin, KServe's path")
	ep = f.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, "https://models.example.com/model-serving/tiny/v1", ep.BaseURL)
	assert.True(t, ep.APIKeyPassthrough, "still routed on the Gateway")
	assert.False(t, ep.PlaceholderAPIKey)

	// A classic InferenceService is not routed on the Gateway: its predictor
	// Service and the placeholder key, as before.
	classic := newFixture(t)
	classic.setDiscoveryOpts(ctx, discoveryOpts{gateway: "https://models.example.com"})
	require.NoError(t, classic.b.Load(ctx, backend.LoadRequest{Name: tinyRepo}))
	loaded, err = classic.b.ListLoaded(ctx)
	require.NoError(t, err)
	require.Len(t, loaded, 1)
	assert.Equal(t, predictorURL("tiny", testServingNS), loaded[0].Endpoint)
	ep = classic.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, predictorURL("tiny", testServingNS)+"/v1", ep.BaseURL)
	assert.True(t, ep.PlaceholderAPIKey)
	assert.False(t, ep.APIKeyPassthrough)
}

func TestServingKindOption(t *testing.T) {
	ctx := context.Background()
	_, err := New(backend.KServeOptions{Clientset: kubefake.NewSimpleClientset(), Dynamic: dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()), ServingKind: "Predictor"})
	assert.ErrorContains(t, err, `serving kind "Predictor"`)

	// Without the LLMInferenceService API, auto composes the classic kind.
	f := newFixture(t)
	s := f.b.cfg.settings(ctx)
	assert.Equal(t, ServingKindClassic, s.ServingKind)
	assert.False(t, s.LLMServed)
	assert.Equal(t, []string{ServingKindClassic}, s.kinds())
	assert.Equal(t, ServingKindClassic, f.b.compose(mustPreset(t, f, "tiny"), s, "").GetKind())

	// The option pins the classic kind although the API is served; the
	// LLMInferenceServices of the namespace are still listed.
	f.b.opts.ServingKind = ServingKindClassic
	f.b.cfg.opts.ServingKind = ServingKindClassic
	f.serveLLMAPI()
	s = f.b.cfg.settings(ctx)
	assert.Equal(t, ServingKindClassic, s.ServingKind)
	assert.True(t, s.LLMServed)
	assert.Equal(t, []string{ServingKindClassic, ServingKindLLM}, s.kinds())
	assert.Equal(t, ServingKindClassic, f.b.compose(mustPreset(t, f, "tiny"), s, "").GetKind())
}
