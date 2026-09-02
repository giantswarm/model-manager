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
	return NewKagent(client, "kagent", "v1alpha2", "", backend.NameOllama), client
}

func ollamaEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Provider: "Ollama", Host: "http://172.21.0.1:11434", Model: model}
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

	require.NoError(t, k.Remove(ctx, "org/qwen3-8b"))
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

	ref, err := k.Lookup(ctx, "qwen3:0.6b")
	require.NoError(t, err)
	require.NotNil(t, ref)
	assert.Equal(t, "qwen3-0-6b", ref.Name)

	missing, err := k.Lookup(ctx, "nope:1b")
	require.NoError(t, err)
	assert.Nil(t, missing)

	all, err := k.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 2)
	assert.Contains(t, all, "smollm2:135m")
	assert.Contains(t, all, "qwen3:0.6b")

	require.NoError(t, k.Remove(ctx, "smollm2:135m"))
	require.NoError(t, k.Remove(ctx, "smollm2:135m"), "removing twice is fine")
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
