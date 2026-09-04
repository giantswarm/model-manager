package wiring

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/giantswarm/model-manager/internal/backend"
)

var testGVR = schema.GroupVersionResource{Group: KagentGroup, Version: "v1alpha2", Resource: ModelConfigResource}

func newFakeKagent(t *testing.T, objs ...runtime.Object) (*Kagent, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme, map[schema.GroupVersionResource]string{
		testGVR:   "ModelConfigList",
		secretGVR: "SecretList",
	}, objs...)
	return NewKagent(client, "kagent", "v1alpha2", ""), client
}

func ollamaEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Backend: backend.NameOllama, Provider: "Ollama", Host: "http://172.21.0.1:11434", Model: model}
}

func lemonadeEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Backend: backend.NameLemonade, Provider: "OpenAI", BaseURL: "http://172.21.0.1:13305/api/v1", Model: model, PlaceholderAPIKey: true}
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
	assert.Equal(t, "kagent.dev/v1alpha2", ref.APIVersion)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "smollm2-135m", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, ManagedByValue, obj.GetLabels()[ManagedByLabel])
	assert.Equal(t, "ollama", obj.GetLabels()[BackendLabel])
	assert.Equal(t, "smollm2:135m", obj.GetAnnotations()[ModelAnnotation])
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "host")
	assert.Equal(t, "http://172.21.0.1:11434", host)
	_, hasKey, _ := unstructured.NestedString(obj.Object, "spec", "apiKeySecret")
	assert.False(t, hasKey, "the native Ollama provider is keyless")

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

	// Simulate the controller setting status.
	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "smollm2-135m", metav1.GetOptions{})
	require.NoError(t, err)
	obj.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Accepted", "status": "True", "message": "Model configuration accepted"}}}
	_, err = client.Resource(testGVR).Namespace("kagent").Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)

	ep := ollamaEndpoint("smollm2:135m")
	ep.Host = "http://10.0.0.1:11434"
	ref, err := k.Ensure(ctx, "smollm2:135m", ep)
	require.NoError(t, err)
	assert.True(t, ref.Ready)
	assert.Equal(t, "Model configuration accepted", ref.Message)

	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "second Ensure must not create a duplicate")
	host, _, _ := unstructured.NestedString(list.Items[0].Object, "spec", "ollama", "host")
	assert.Equal(t, "http://10.0.0.1:11434", host, "spec is refreshed")
}

func TestEnsureRefusesForeignModelConfig(t *testing.T) {
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kagent.dev/v1alpha2",
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

	// The backend now names the ModelConfig after the InferenceService.
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
	kp := NewKagent(client, "kagent", "v1alpha2", "mm")
	pref, err := kp.Ensure(ctx, "Inferact/Qwen3.8-27B-NVFP4", ep)
	require.NoError(t, err)
	assert.Equal(t, "mm-qwen3-8-27b", pref.Name)
}

func TestListAllReportsForeignModelConfigs(t *testing.T) {
	portal := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kagent.dev/v1alpha2",
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
		"apiVersion": "kagent.dev/v1alpha2",
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
