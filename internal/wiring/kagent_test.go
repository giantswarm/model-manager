package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	discoveryfake "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/openapi"
	"k8s.io/client-go/openapi/openapitest"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/giantswarm/model-manager/internal/backend"
)

// testGVR is the ModelConfig resource in the version kagent API v2 serves.
var testGVR = schema.GroupVersionResource{Group: KagentGroup, Version: DefaultAPIVersion, Resource: ModelConfigResource}

// testAPIVersion is the apiVersion of the objects the fakes hold.
const testAPIVersion = KagentGroup + "/" + DefaultAPIVersion

func newFakeKagent(t *testing.T, objs ...runtime.Object) (*Kagent, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		testGVR:   "ModelConfigList",
		secretGVR: "SecretList",
	}, objs...)
	return NewKagent(client, servedOpenAPI(DefaultAPIVersion, kagentOllamaFields...), "kagent", DefaultAPIVersion, ""), client
}

// kagentOllamaFields are the spec.ollama fields of the ModelConfig kagent
// 1.0.3 serves; kagent 1.0.2 serves them without think.
var kagentOllamaFields = []string{"host", "options", "think"}

// servedOpenAPI is the apiserver's OpenAPI v3 for kagent.dev/<version>, in
// the shape it publishes a CRD in: a component per kind, marked with its
// group, version and kind, the structural schema inlined.
func servedOpenAPI(version string, ollamaFields ...string) openapi.ClientWithContext {
	ollama := map[string]any{}
	for _, f := range ollamaFields {
		ollama[f] = map[string]any{"type": "string"}
	}
	component := func(kind string) map[string]any {
		return map[string]any{
			"type":                            "object",
			"x-kubernetes-group-version-kind": []any{map[string]any{"group": KagentGroup, "version": version, "kind": kind}},
			"properties": map[string]any{
				"metadata": map[string]any{"allOf": []any{map[string]any{"$ref": "#/components/schemas/io.k8s.apimachinery.pkg.apis.meta.v1.ObjectMeta"}}},
				"spec":     map[string]any{"type": "object", "properties": map[string]any{"ollama": map[string]any{"type": "object", "properties": ollama}}},
			},
		}
	}
	raw, err := json.Marshal(map[string]any{"openapi": "3.0.0", "components": map[string]any{"schemas": map[string]any{
		"dev.kagent." + version + ".Agent":           map[string]any{"type": "object", "x-kubernetes-group-version-kind": []any{map[string]any{"group": KagentGroup, "version": version, "kind": "Agent"}}},
		"dev.kagent." + version + ".ModelConfig":     component("ModelConfig"),
		"dev.kagent." + version + ".ModelConfigList": map[string]any{"type": "object", "x-kubernetes-group-version-kind": []any{map[string]any{"group": KagentGroup, "version": version, "kind": "ModelConfigList"}}},
	}}})
	if err != nil {
		panic(err)
	}
	return openapi.ToClientWithContext(&openapitest.FakeClient{PathsMap: map[string]openapi.GroupVersion{
		"apis/" + KagentGroup + "/" + version: openapitest.FakeGroupVersion{GVSpec: raw},
	}})
}

func ptr[T any](v T) *T { return &v }

func ollamaEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Backend: backend.NameOllama, Provider: "Ollama", Host: "http://172.21.0.1:11434", Model: model, ContextLength: 32768, Think: ptr(false)}
}

func lemonadeEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Backend: backend.NameLemonade, Provider: "OpenAI", BaseURL: "http://172.21.0.1:13305/api/v1", Model: model, PlaceholderAPIKey: true}
}

// gatewayEndpoint is what the kserve backend answers for a model routed on
// the models Gateway: the caller's token forwarded, no placeholder.
func gatewayEndpoint(name string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Backend: backend.NameKServe, Provider: "OpenAI", BaseURL: "https://models.example.com/model-serving/" + name + "/v1", Model: name, Name: name, APIKeyPassthrough: true}
}

// kservePredictorEndpoint is the same model reached on its in-cluster
// Service: keyless vLLM behind kagent's OpenAI provider, placeholder key.
func kservePredictorEndpoint(name string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Backend: backend.NameKServe, Provider: "OpenAI", BaseURL: "http://" + name + "-predictor.model-serving.svc.cluster.local/v1", Model: name, Name: name, PlaceholderAPIKey: true}
}

func TestEnsureCreatesNativeOllamaModelConfig(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()

	ref, err := k.Ensure(ctx, "smollm2:135m", ollamaEndpoint("smollm2:135m"))
	require.NoError(t, err)
	assert.Equal(t, "smollm2-135m", ref.Name)
	assert.Equal(t, "kagent", ref.Namespace)
	assert.Equal(t, "Ollama", ref.Provider)
	assert.Equal(t, "smollm2:135m", ref.Model)
	assert.Equal(t, backend.NameOllama, ref.Backend, "the ref carries the backend label")
	assert.False(t, ref.Ready, "no controller has reconciled yet")
	assert.Equal(t, "kagent.dev/v1alpha3", ref.APIVersion)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "smollm2-135m", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ManagedByValue, obj.GetLabels()[ManagedByLabel])
	assert.Equal(t, "ollama", obj.GetLabels()[BackendLabel])
	assert.Equal(t, "smollm2:135m", obj.GetAnnotations()[ModelAnnotation])
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "host")
	assert.Equal(t, "http://172.21.0.1:11434", host)
	_, hasKey, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	assert.False(t, hasKey, "the native Ollama provider is keyless")
	numCtx, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "options", "num_ctx")
	assert.Equal(t, "32768", numCtx, "the context window rides on every request, not the server's VRAM-tiered default")
	assert.Equal(t, int64(32768), ref.ContextLength, "the ref reports the window agents run at")
	think, found, _ := unstructured.NestedBool(obj.Object, "spec", "ollama", "think")
	assert.True(t, found, "think rides on every request of a thinking model")
	assert.False(t, think)
	require.NotNil(t, ref.Think, "the ref reports think")
	assert.False(t, *ref.Think)

	// No placeholder secret for Ollama.
	secrets, err := client.Resource(secretGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secrets.Items)
}

func TestEnsureIsIdempotentAndUpdates(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	_, err := k.Ensure(ctx, "smollm2:135m", ollamaEndpoint("smollm2:135m"))
	require.NoError(t, err)

	// Simulate the controller setting status (kagent API v2: Accepted and
	// ResolvedRefs, both True).
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "smollm2-135m", metav1.GetOptions{})
	require.NoError(t, err)
	obj.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Accepted", "status": "True", "reason": "Accepted", "message": "ModelConfig configuration accepted"},
		map[string]any{"type": "ResolvedRefs", "status": "True", "reason": "Resolved", "message": "All referenced secrets and config maps resolved"},
	}}
	_, err = client.Resource(testGVR).Namespace("kagent").Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)

	ep := ollamaEndpoint("smollm2:135m")
	ep.Host = "http://10.0.0.1:11434"
	ref, err := k.Ensure(ctx, "smollm2:135m", ep)
	require.NoError(t, err)
	assert.True(t, ref.Ready)
	assert.Equal(t, "ModelConfig configuration accepted", ref.Message)

	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "second Ensure must not create a duplicate")
	host, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "ollama", "host")
	assert.Equal(t, "http://10.0.0.1:11434", host, "spec is refreshed")
}

// TestEnsureOllamaContextLength: the ModelConfig carries the endpoint's
// context window and follows it on a re-wire — a num_ctx set by hand is
// replaced — and an endpoint without one leaves the options out.
func TestEnsureOllamaContextLength(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	numCtx := func() (string, bool) {
		t.Helper()
		obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b", metav1.GetOptions{})
		require.NoError(t, err)
		v, found, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "options", "num_ctx")
		return v, found
	}

	_, err := k.Ensure(ctx, "qwen3:0.6b", ollamaEndpoint("qwen3:0.6b"))
	require.NoError(t, err)
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(obj.Object, "2048", "spec", "ollama", "options", "num_ctx"))
	_, err = client.Resource(testGVR).Namespace("kagent").Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	ref, err := k.Lookup(ctx, backend.NameOllama, "qwen3:0.6b")
	require.NoError(t, err)
	assert.Equal(t, int64(2048), ref.ContextLength, "Lookup reports what the ModelConfig says")

	ep := ollamaEndpoint("qwen3:0.6b")
	ep.ContextLength = 16384
	ref, err = k.Ensure(ctx, "qwen3:0.6b", ep)
	require.NoError(t, err)
	assert.Equal(t, int64(16384), ref.ContextLength)
	v, _ := numCtx()
	assert.Equal(t, "16384", v, "a re-wire writes the endpoint's window over the hand-set one")

	ep.ContextLength = 0
	ref, err = k.Ensure(ctx, "qwen3:0.6b", ep)
	require.NoError(t, err)
	assert.Zero(t, ref.ContextLength)
	_, found := numCtx()
	assert.False(t, found, "no window: the server's default applies")
}

// TestEnsureOllamaThink: the ModelConfig carries the endpoint's think and
// follows it on a re-wire — a think set by hand is replaced — and an endpoint
// without one leaves the field out.
func TestEnsureOllamaThink(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	think := func() (bool, bool) {
		t.Helper()
		obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-5-2b", metav1.GetOptions{})
		require.NoError(t, err)
		v, found, _ := unstructured.NestedBool(obj.Object, "spec", "ollama", "think")
		return v, found
	}

	_, err := k.Ensure(ctx, "qwen3.5:2b", ollamaEndpoint("qwen3.5:2b"))
	require.NoError(t, err)
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-5-2b", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(obj.Object, true, "spec", "ollama", "think"))
	_, err = client.Resource(testGVR).Namespace("kagent").Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	ref, err := k.Lookup(ctx, backend.NameOllama, "qwen3.5:2b")
	require.NoError(t, err)
	require.NotNil(t, ref.Think)
	assert.True(t, *ref.Think, "Lookup reports what the ModelConfig says")
	assert.False(t, ref.Carries(ollamaEndpoint("qwen3.5:2b")), "a hand-set think is not what the endpoint asks for")

	ref, err = k.Ensure(ctx, "qwen3.5:2b", ollamaEndpoint("qwen3.5:2b"))
	require.NoError(t, err)
	v, found := think()
	assert.True(t, found)
	assert.False(t, v, "a re-wire writes the endpoint's think over the hand-set one")
	assert.True(t, ref.Carries(ollamaEndpoint("qwen3.5:2b")))

	ep := ollamaEndpoint("qwen3.5:2b")
	ep.Think = nil
	ref, err = k.Ensure(ctx, "qwen3.5:2b", ep)
	require.NoError(t, err)
	assert.Nil(t, ref.Think)
	_, found = think()
	assert.False(t, found, "no think: the server's default applies")
	assert.True(t, ref.Carries(ep))
}

// TestThinkIsLeftOutWhereTheServedSchemaLacksIt: on a kagent whose
// ModelConfig has no spec.ollama.think (before 1.0.3) the apiserver would
// prune the field, so the wirer never writes it, Writable says so, and what a
// ModelConfig reads back carries the writable endpoint — no re-wire on every
// pass. Once the served schema has the field (after schemaTTL), think is
// written.
func TestThinkIsLeftOutWhereTheServedSchemaLacksIt(t *testing.T) {
	k, client := newFakeKagent(t)
	now := time.Now()
	k.schema = &servedSchema{client: servedOpenAPI(DefaultAPIVersion, "host", "options"), version: DefaultAPIVersion, now: func() time.Time { return now }}
	ctx := context.Background()

	ep := ollamaEndpoint("qwen3.5:2b")
	want, err := k.Writable(ctx, ep)
	require.NoError(t, err)
	assert.Nil(t, want.Think, "the served schema has no think")
	assert.Equal(t, ep.ContextLength, want.ContextLength, "the rest of the endpoint is written as it is")
	ref, err := k.Ensure(ctx, "qwen3.5:2b", ep)
	require.NoError(t, err)
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-5-2b", metav1.GetOptions{})
	require.NoError(t, err)
	_, found, _ := unstructured.NestedBool(obj.Object, "spec", "ollama", "think")
	assert.False(t, found, "a field the CRD lacks is not written")
	assert.True(t, ref.Carries(want), "the written ModelConfig carries what is writable: nothing to re-wire")
	assert.False(t, ref.Carries(ep), "compared with the unfitted endpoint it would re-wire on every pass")

	// kagent upgraded: the schema serves think from the next check on.
	k.schema.client = servedOpenAPI(DefaultAPIVersion, kagentOllamaFields...)
	want, err = k.Writable(ctx, ep)
	require.NoError(t, err)
	assert.Nil(t, want.Think, "the answer holds for schemaTTL")
	now = now.Add(schemaTTL)
	want, err = k.Writable(ctx, ep)
	require.NoError(t, err)
	require.NotNil(t, want.Think, "after schemaTTL the served schema is read again")
	ref, err = k.Ensure(ctx, "qwen3.5:2b", ep)
	require.NoError(t, err)
	require.NotNil(t, ref.Think)
	assert.False(t, *ref.Think)
}

// TestWritableConsultsTheSchemaOnlyForThink: an endpoint without think never
// reads the served schema, so wiring without the setting works whatever the
// schema says; one with think fails with the reason when the schema cannot
// be read, instead of writing a field the apiserver may prune.
func TestWritableConsultsTheSchemaOnlyForThink(t *testing.T) {
	k, _ := newFakeKagent(t)
	k.schema = &servedSchema{client: openapi.ToClientWithContext(&openapitest.FakeClient{ForcedErr: errors.New("openapi down")}), version: DefaultAPIVersion, now: time.Now}
	ctx := context.Background()

	for _, ep := range []backend.AgentEndpoint{lemonadeEndpoint("qwen3-4b-FLM"), func() backend.AgentEndpoint { ep := ollamaEndpoint("smollm2:135m"); ep.Think = nil; return ep }()} {
		_, err := k.Ensure(ctx, ep.Model, ep)
		require.NoError(t, err, "%s: no think, no schema read", ep.Model)
	}
	_, err := k.Ensure(ctx, "qwen3.5:2b", ollamaEndpoint("qwen3.5:2b"))
	require.ErrorContains(t, err, "openapi down")

	k.schema = &servedSchema{client: openapi.ToClientWithContext(openapitest.NewFakeClient()), version: DefaultAPIVersion, now: time.Now}
	_, err = k.Ensure(ctx, "qwen3.5:2b", ollamaEndpoint("qwen3.5:2b"))
	require.ErrorContains(t, err, "publishes no OpenAPI v3 schema for kagent.dev/v1alpha3")
}

func TestCarriesComparesTheAgentSettings(t *testing.T) {
	ep := ollamaEndpoint("qwen3.5:2b")
	cases := map[string]struct {
		ref  ModelConfigRef
		want bool
	}{
		"same window, same think": {ModelConfigRef{ContextLength: 32768, Think: ptr(false)}, true},
		"another window":          {ModelConfigRef{ContextLength: 4096, Think: ptr(false)}, false},
		"think unset":             {ModelConfigRef{ContextLength: 32768}, false},
		"think on":                {ModelConfigRef{ContextLength: 32768, Think: ptr(true)}, false},
		"host and shape ignored":  {ModelConfigRef{ContextLength: 32768, Think: ptr(false), Endpoint: "http://elsewhere:11434", APIKeySecret: "x"}, true},
	}
	for name, tc := range cases {
		assert.Equal(t, tc.want, tc.ref.Carries(ep), name)
	}
	assert.True(t, ModelConfigRef{}.Carries(backend.AgentEndpoint{}), "nothing asked, nothing written")
}

func TestEnsureRefusesForeignModelConfig(t *testing.T) {
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": testAPIVersion,
		"kind":       "ModelConfig",
		"metadata":   map[string]any{"name": "smollm2-135m", "namespace": "kagent", "labels": map[string]any{ManagedByLabel: "agentlab"}},
		"spec":       map[string]any{"provider": "OpenAI", "model": "smollm2:135m"},
	}}
	k, _ := newFakeKagent(t, foreign)
	_, err := k.Ensure(context.Background(), "smollm2:135m", ollamaEndpoint("smollm2:135m"))
	require.ErrorIs(t, err, backend.ErrConflict)
}

func TestEnsureOpenAIPlaceholderSecret(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	ep := backend.AgentEndpoint{Provider: "OpenAI", BaseURL: "http://vllm.svc/v1", Model: "qwen3-8b", PlaceholderAPIKey: true}
	ref, err := k.Ensure(ctx, "org/qwen3-8b", ep)
	require.NoError(t, err)
	assert.Equal(t, "org-qwen3-8b", ref.Name)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "org-qwen3-8b", metav1.GetOptions{})
	require.NoError(t, err)
	secretName, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	assert.Equal(t, "org-qwen3-8b-api-key", secretName)
	key, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecretKey")
	assert.Equal(t, "OPENAI_API_KEY", key)
	base, _, _ := unstructured.NestedString(obj.Object, "spec", "openAI", "baseUrl")
	assert.Equal(t, "http://vllm.svc/v1", base)

	sec, err := client.Resource(secretGVR).Namespace("kagent").Get(ctx, "org-qwen3-8b-api-key", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ManagedByValue, sec.GetLabels()[ManagedByLabel])

	require.NoError(t, k.Remove(ctx, "", "org/qwen3-8b"))
	_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "org-qwen3-8b-api-key", metav1.GetOptions{})
	require.Error(t, err, "placeholder secret goes with the ModelConfig")
}

func TestEnsurePassthroughReferencesNoSecret(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	ref, err := k.Ensure(ctx, "Qwen/Qwen3-4B-Instruct-2507", gatewayEndpoint("qwen3-4b-instruct"))
	require.NoError(t, err)
	assert.Equal(t, "qwen3-4b-instruct", ref.Name)
	assert.True(t, ref.APIKeyPassthrough, "the ref reports the shape")
	assert.Empty(t, ref.APIKeySecret)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-4b-instruct", metav1.GetOptions{})
	require.NoError(t, err)
	passthrough, _, _ := unstructured.NestedBool(obj.Object, "spec", "apiKeyPassthrough")
	assert.True(t, passthrough, "the agent forwards the caller's token to the Gateway")
	_, hasSecret, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	assert.False(t, hasSecret, "no placeholder key: the Gateway would answer 401 to it")
	_, hasKey, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecretKey")
	assert.False(t, hasKey)
	secrets, err := client.Resource(secretGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secrets.Items, "no placeholder Secret is created for the passthrough shape")

	require.NoError(t, k.Remove(ctx, backend.NameKServe, "Qwen/Qwen3-4B-Instruct-2507"))
	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
}

func TestEnsureRefusesPassthroughWithSecret(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	ep := gatewayEndpoint("qwen3-4b-instruct")
	ep.APIKeySecret = "my-key"
	_, err := k.Ensure(ctx, "Qwen/Qwen3-4B-Instruct-2507", ep)
	require.ErrorIs(t, err, backend.ErrInvalid)
	assert.ErrorContains(t, err, "mutually exclusive")
	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items, "nothing is written for a shape the CRD refuses")
}

func TestEnsureCallerSecretIsReferencedNotCreated(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	ep := lemonadeEndpoint("qwen3-4b-FLM")
	ep.APIKeySecret = "lemonade-key"
	ref, err := k.Ensure(ctx, "qwen3-4b-FLM", ep)
	require.NoError(t, err)
	assert.Equal(t, "lemonade-key", ref.APIKeySecret)
	assert.False(t, ref.APIKeyPassthrough)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-4b-flm", metav1.GetOptions{})
	require.NoError(t, err)
	secretName, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	assert.Equal(t, "lemonade-key", secretName, "the caller's Secret replaces the placeholder")
	key, _, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecretKey")
	assert.Equal(t, "OPENAI_API_KEY", key, "the key defaults when the caller names none")
	secrets, err := client.Resource(secretGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, secrets.Items, "a Secret of the caller's is never created")
	require.NoError(t, k.Remove(ctx, backend.NameLemonade, "qwen3-4b-FLM"), "and never deleted")
}

func TestEnsureRewireToPassthroughRemovesPlaceholderSecret(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	const model = "Qwen/Qwen3-4B-Instruct-2507"
	_, err := k.Ensure(ctx, model, kservePredictorEndpoint("qwen3-4b-instruct"))
	require.NoError(t, err)
	_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "qwen3-4b-instruct-api-key", metav1.GetOptions{})
	require.NoError(t, err, "the in-cluster shape has its placeholder Secret")

	// KServe publishes the route; the next wire moves the ModelConfig onto
	// the Gateway's contract and the placeholder goes with the old shape.
	ref, err := k.Ensure(ctx, model, gatewayEndpoint("qwen3-4b-instruct"))
	require.NoError(t, err)
	assert.True(t, ref.APIKeyPassthrough)
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-4b-instruct", metav1.GetOptions{})
	require.NoError(t, err)
	_, hasSecret, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	assert.False(t, hasSecret, "the refreshed spec carries no apiKeySecret")
	_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "qwen3-4b-instruct-api-key", metav1.GetOptions{})
	require.Error(t, err, "the stale placeholder Secret is removed with the shape")
	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "one ModelConfig, refreshed in place")
}

func TestLookupListRemove(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	_, err := k.Ensure(ctx, "smollm2:135m", ollamaEndpoint("smollm2:135m"))
	require.NoError(t, err)
	_, err = k.Ensure(ctx, "qwen3:0.6b", ollamaEndpoint("qwen3:0.6b"))
	require.NoError(t, err)

	ref, err := k.Lookup(ctx, backend.NameOllama, "qwen3:0.6b")
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "qwen3-0-6b", ref.Name)

	missing, err := k.Lookup(ctx, backend.NameOllama, "nope:1b")
	require.NoError(t, err)
	assert.Nil(t, missing)
	other, err := k.Lookup(ctx, backend.NameLemonade, "qwen3:0.6b")
	require.NoError(t, err)
	assert.Nil(t, other, "another backend's ModelConfig is not this backend's")

	all, err := k.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	models := map[string]backend.Name{}
	for _, r := range all {
		models[r.Model] = r.Backend
	}
	assert.Equal(t, backend.NameOllama, models["smollm2:135m"])
	assert.Equal(t, backend.NameOllama, models["qwen3:0.6b"])

	require.NoError(t, k.Remove(ctx, backend.NameOllama, "smollm2:135m"))
	require.NoError(t, k.Remove(ctx, backend.NameOllama, "smollm2:135m"), "removing twice is fine")
	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "qwen3-0-6b", list.Items[0].GetName())
}

func TestModelConfigName(t *testing.T) {
	cases := map[string]string{
		"smollm2:135m":                           "smollm2-135m",
		"qwen3:0.6b":                             "qwen3-0-6b",
		"hf.co/bartowski/Qwen2.5-7B-GGUF:Q4_K_M": "hf-co-bartowski-qwen2-5-7b-gguf-q4-k-m",
		"gemma3:latest":                          "gemma3-latest",
		"7b-model":                               "m-7b-model",
		"--weird__name--":                        "weird-name",
		"":                                       "model",
	}
	for in, want := range cases {
		assert.Equal(t, want, ModelConfigName("", in), in)
	}
	assert.Equal(t, "mm-smollm2-135m", ModelConfigName("mm", "smollm2:135m"))
	assert.Equal(t, "mm-smollm2-135m", ModelConfigName("mm-", "smollm2:135m"))

	long := ModelConfigName("", "hf.co/"+strings.Repeat("a", 80)+"/repo:Q8")
	assert.LessOrEqual(t, len(long), 63)
	other := ModelConfigName("", "hf.co/"+strings.Repeat("a", 80)+"/repo:Q4")
	assert.NotEqual(t, long, other, "long names stay distinct through the hash suffix")
}

func TestEnsureUsesTheEndpointNameAndConverges(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	// An older naming rule left a ModelConfig derived from the repository id.
	old, err := k.Ensure(ctx, "Inferact/Qwen3.8-27B-NVFP4", backend.AgentEndpoint{Provider: "OpenAI", BaseURL: "http://qwen3-8-27b-predictor.model-serving.svc.cluster.local/v1", Model: "qwen3-8-27b", PlaceholderAPIKey: true})
	require.NoError(t, err)
	assert.Equal(t, "inferact-qwen3-8-27b-nvfp4", old.Name)

	// The backend now names the ModelConfig after the LLMInferenceService.
	ep := backend.AgentEndpoint{Provider: "OpenAI", BaseURL: "http://qwen3-8-27b-predictor.model-serving.svc.cluster.local/v1", Model: "qwen3-8-27b", PlaceholderAPIKey: true, Name: "qwen3-8-27b"}
	ref, err := k.Ensure(ctx, "Inferact/Qwen3.8-27B-NVFP4", ep)
	require.NoError(t, err)
	assert.Equal(t, "qwen3-8-27b", ref.Name)
	assert.Equal(t, "Inferact/Qwen3.8-27B-NVFP4", ref.Model)
	assert.Equal(t, "qwen3-8-27b", ref.ProviderModel)
	assert.Equal(t, "http://qwen3-8-27b-predictor.model-serving.svc.cluster.local/v1", ref.Endpoint)
	assert.True(t, ref.Managed)

	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "the old name was replaced, not duplicated")
	assert.Equal(t, "qwen3-8-27b", list.Items[0].GetName())
	_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "inferact-qwen3-8-27b-nvfp4-api-key", metav1.GetOptions{})
	assert.Error(t, err, "the old placeholder secret went with it")
	sec, err := client.Resource(secretGVR).Namespace("kagent").Get(ctx, "qwen3-8-27b-api-key", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ManagedByValue, sec.GetLabels()[ManagedByLabel])

	// Idempotent under the new name; Remove finds it by the model annotation.
	again, err := k.Ensure(ctx, "Inferact/Qwen3.8-27B-NVFP4", ep)
	require.NoError(t, err)
	assert.Equal(t, "qwen3-8-27b", again.Name)
	require.NoError(t, k.Remove(ctx, "", "Inferact/Qwen3.8-27B-NVFP4"))
	list, err = client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)

	// A prefix still applies to backend-chosen names.
	kp := NewKagent(client, servedOpenAPI(DefaultAPIVersion, kagentOllamaFields...), "kagent", DefaultAPIVersion, "mm")
	pref, err := kp.Ensure(ctx, "Inferact/Qwen3.8-27B-NVFP4", ep)
	require.NoError(t, err)
	assert.Equal(t, "mm-qwen3-8-27b", pref.Name)
}

func TestListAllReportsForeignModelConfigs(t *testing.T) {
	portal := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": testAPIVersion,
		"kind":       "ModelConfig",
		"metadata":   map[string]any{"name": "qwen3-8-27b", "namespace": "kagent", "labels": map[string]any{ManagedByLabel: "backstage"}},
		"spec": map[string]any{"provider": "OpenAI", "model": "qwen3-8-27b", "apiKeySecret": "qwen3-8-27b-key", "apiKeySecretKey": "OPENAI_API_KEY", // #nosec G101 -- Secret name and key, not a credential
			"openAI": map[string]any{"baseUrl": "http://qwen3-8-27b-predictor.model-serving.svc.cluster.local/v1"}},
	}}
	k, _ := newFakeKagent(t, portal)
	ctx := context.Background()
	_, err := k.Ensure(ctx, "smollm2:135m", ollamaEndpoint("smollm2:135m"))
	require.NoError(t, err)

	all, err := k.ListAll(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	byName := map[string]ModelConfigRef{}
	for _, r := range all {
		byName[r.Name] = r
	}
	assert.False(t, byName["qwen3-8-27b"].Managed)
	assert.Equal(t, "qwen3-8-27b", byName["qwen3-8-27b"].ProviderModel)
	assert.Equal(t, "http://qwen3-8-27b-predictor.model-serving.svc.cluster.local/v1", byName["qwen3-8-27b"].Endpoint)
	assert.True(t, byName["smollm2-135m"].Managed)
	assert.Equal(t, "http://172.21.0.1:11434", byName["smollm2-135m"].Endpoint)

	owned, err := k.List(ctx)
	require.NoError(t, err)
	assert.Len(t, owned, 1, "List stays model-manager's own")
	require.NoError(t, k.Remove(ctx, "", "qwen3-8-27b"), "absent from the owned set: a no-op")
	all, err = k.ListAll(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2, "foreign ModelConfigs are never deleted")
}

func TestSameReferenceOnTwoBackendsIsTwoModelConfigs(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()

	// The first backend keeps the plain derived name.
	first, err := k.Ensure(ctx, "shared:1b", ollamaEndpoint("shared:1b"))
	require.NoError(t, err)
	assert.Equal(t, "shared-1b", first.Name)
	assert.Equal(t, backend.NameOllama, first.Backend)

	// The same reference on another backend: the derived name is taken by a
	// managed ModelConfig of another backend, so this one carries its backend.
	second, err := k.Ensure(ctx, "shared:1b", lemonadeEndpoint("shared:1b"))
	require.NoError(t, err)
	assert.Equal(t, "shared-1b-lemonade", second.Name)
	assert.Equal(t, backend.NameLemonade, second.Backend)
	assert.Equal(t, "shared:1b", second.Model)

	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 2, "one ModelConfig per (backend, model)")

	// Idempotent per backend, and each side finds only its own.
	again, err := k.Ensure(ctx, "shared:1b", lemonadeEndpoint("shared:1b"))
	require.NoError(t, err)
	assert.Equal(t, "shared-1b-lemonade", again.Name)
	list, err = client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 2, "a repeated Ensure neither duplicates nor renames")
	ol, err := k.Lookup(ctx, backend.NameOllama, "shared:1b")
	require.NoError(t, err)
	require.NotNil(t, ol)
	assert.Equal(t, "shared-1b", ol.Name)
	le, err := k.Lookup(ctx, backend.NameLemonade, "shared:1b")
	require.NoError(t, err)
	require.NotNil(t, le)
	assert.Equal(t, "shared-1b-lemonade", le.Name)

	// Remove is per backend: the other backend's ModelConfig stays.
	require.NoError(t, k.Remove(ctx, backend.NameOllama, "shared:1b"))
	list, err = client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
	assert.Equal(t, "shared-1b-lemonade", list.Items[0].GetName())
	_, err = client.Resource(secretGVR).Namespace("kagent").Get(ctx, "shared-1b-lemonade-api-key", metav1.GetOptions{})
	require.NoError(t, err, "the lemonade placeholder secret survives the ollama unwire")

	// With the plain name free again, the lemonade ModelConfig keeps its
	// suffixed name: converging would delete and recreate it for nothing.
	again, err = k.Ensure(ctx, "shared:1b", lemonadeEndpoint("shared:1b"))
	require.NoError(t, err)
	assert.Equal(t, "shared-1b-lemonade", again.Name)
}

func TestLegacyModelConfigWithoutBackendLabelMatchesAnyBackend(t *testing.T) {
	legacy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": testAPIVersion,
		"kind":       "ModelConfig",
		"metadata": map[string]any{"name": "old-1b", "namespace": "kagent",
			"labels":      map[string]any{ManagedByLabel: ManagedByValue},
			"annotations": map[string]any{ModelAnnotation: "old:1b"}},
		"spec": map[string]any{"provider": "Ollama", "model": "old:1b", "ollama": map[string]any{"host": "http://172.21.0.1:11434"}},
	}}
	k, client := newFakeKagent(t, legacy)
	ctx := context.Background()

	ref, err := k.Lookup(ctx, backend.NameOllama, "old:1b")
	require.NoError(t, err)
	require.NotNil(t, ref, "a ModelConfig written before the backend label existed belongs to whichever backend asks")
	assert.Equal(t, backend.Name(""), ref.Backend)

	// Ensure adopts it under the backend's label instead of creating a second one.
	updated, err := k.Ensure(ctx, "old:1b", ollamaEndpoint("old:1b"))
	require.NoError(t, err)
	assert.Equal(t, "old-1b", updated.Name)
	assert.Equal(t, backend.NameOllama, updated.Backend)
	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1)
}

// apiResources is one served group/version with the named resources, as
// discovery lists it. The fake's preferred version is the first one listed.
func apiResources(groupVersion string, names ...string) *metav1.APIResourceList {
	l := &metav1.APIResourceList{GroupVersion: groupVersion}
	for _, n := range names {
		l.APIResources = append(l.APIResources, metav1.APIResource{Name: n})
	}
	return l
}

func TestDiscoverAPIVersionFollowsWhatTheClusterServes(t *testing.T) {
	cases := map[string]struct {
		served  []*metav1.APIResourceList
		want    string
		wantErr string
	}{
		"kagent API v2 serves v1alpha3 only": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "agenttemplates", "modelconfigs")},
			want:   "v1alpha3",
		},
		"kagent 0.x prefers v1alpha2 and still serves v1alpha1": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha2", "agents", "modelconfigs"), apiResources("kagent.dev/v1alpha1", "agents", "modelconfigs")},
			want:   "v1alpha2",
		},
		"the preferred version lacks the resource, another has it": {
			served: []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "agenttemplates"), apiResources("kagent.dev/v1alpha2", "modelconfigs")},
			want:   "v1alpha2",
		},
		"the group serves no modelconfigs": {
			served:  []*metav1.APIResourceList{apiResources("kagent.dev/v1alpha3", "agenttemplates")},
			wantErr: "has no modelconfigs resource",
		},
		"kagent is not installed": {
			served:  []*metav1.APIResourceList{apiResources("apps/v1", "deployments")},
			wantErr: "API group kagent.dev not found",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dc := &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: tc.served}}
			got, err := DiscoverAPIVersion(dc)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDefaultAPIVersionIsTheV2One: what the service runs on when discovery
// fails (cmd/serve falls back to DefaultAPIVersion) or is bypassed.
func TestDefaultAPIVersionIsTheV2One(t *testing.T) {
	assert.Equal(t, "v1alpha3", DefaultAPIVersion)
	k := NewKagent(nil, nil, "kagent", "", "")
	assert.Equal(t, DefaultAPIVersion, k.APIVersion())
}

func TestReadyNeedsAcceptedAndResolvedRefs(t *testing.T) {
	cond := func(typ, status, message string) map[string]any {
		return map[string]any{"type": typ, "status": status, "message": message}
	}
	cases := map[string]struct {
		conditions []any
		ready      bool
		message    string
	}{
		"no status yet": {nil, false, "not yet reconciled"},
		"accepted only (kagent 0.x, v1alpha2)": {
			[]any{cond("Accepted", "True", "Model configuration accepted")}, true, "Model configuration accepted"},
		"accepted and resolved (kagent API v2, v1alpha3)": {
			[]any{cond("Accepted", "True", "ModelConfig configuration accepted"), cond("ResolvedRefs", "True", "All referenced secrets and config maps resolved")},
			true, "ModelConfig configuration accepted"},
		"accepted but the secret is missing": {
			[]any{cond("Accepted", "True", "ModelConfig configuration accepted"), cond("ResolvedRefs", "False", "secret qwen3-4b-flm-api-key not found")},
			false, "secret qwen3-4b-flm-api-key not found"},
		"accepted, references not resolved yet": {
			[]any{cond("Accepted", "True", "ModelConfig configuration accepted"), cond("ResolvedRefs", "Unknown", "resolving references")},
			false, "resolving references"},
		"rejected spec": {
			[]any{cond("Accepted", "False", "ollama model config is required"), cond("ResolvedRefs", "True", "All referenced secrets and config maps resolved")},
			false, "ollama model config is required"},
		"resolved but never accepted": {
			[]any{cond("ResolvedRefs", "True", "All referenced secrets and config maps resolved")}, false, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			obj := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": testAPIVersion,
				"kind":       "ModelConfig",
				"metadata":   map[string]any{"name": "m", "namespace": "kagent"},
				"spec":       map[string]any{"provider": "Ollama", "model": "m"},
			}}
			if tc.conditions != nil {
				obj.Object["status"] = map[string]any{"conditions": tc.conditions}
			}
			ref := toRef(obj)
			assert.Equal(t, tc.ready, ref.Ready)
			assert.Equal(t, tc.message, ref.Message)
		})
	}
}

// TestBuildPinsTheV1alpha3Shape compares what Ensure writes for a keyless
// Ollama model and an OpenAI-compatible one with testdata/*.yaml — the files
// `kubectl -n kagent create --dry-run=server -f` validates against a kagent
// API v2 cluster (v1alpha3: the provider-specific block only with its
// provider, apiKeySecret and apiKeySecretKey together, apiKeyPassthrough
// without either). UPDATE_GOLDEN=1 rewrites them.
func TestBuildPinsTheV1alpha3Shape(t *testing.T) {
	k, _ := newFakeKagent(t)
	goldens := map[string]*unstructured.Unstructured{
		"modelconfig-v1alpha3-ollama.yaml":             k.build("qwen2-5-0-5b", "qwen2.5:0.5b", ollamaEndpoint("qwen2.5:0.5b")),
		"modelconfig-v1alpha3-openai.yaml":             k.build("qwen3-4b-flm", "qwen3-4b-FLM", lemonadeEndpoint("qwen3-4b-FLM")),
		"modelconfig-v1alpha3-openai-secret.yaml":      k.placeholderSecret("qwen3-4b-flm"),
		"modelconfig-v1alpha3-openai-passthrough.yaml": k.build("qwen3-4b-instruct", "Qwen/Qwen3-4B-Instruct-2507", gatewayEndpoint("qwen3-4b-instruct")),
	}
	for file, obj := range goldens {
		t.Run(file, func(t *testing.T) {
			got, err := yaml.Marshal(obj.Object)
			require.NoError(t, err)
			path := filepath.Join("testdata", file)
			if os.Getenv("UPDATE_GOLDEN") != "" {
				require.NoError(t, os.WriteFile(path, got, 0o600))
			}
			want, err := os.ReadFile(path) //nolint:gosec // a golden file under testdata, named by the test
			require.NoError(t, err)
			assert.Equal(t, string(want), string(got))
		})
	}
}
