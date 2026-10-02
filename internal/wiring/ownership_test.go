package wiring

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

// testInstance is the instance the fakes' wirer runs as.
const testInstance = "kagent-model-manager"

// managedModelConfig is a ModelConfig labelled managed by model-manager for
// model on ollama, created by instance (empty: no instance label).
func managedModelConfig(name, model, instance string) *unstructured.Unstructured {
	labels := map[string]any{ManagedByLabel: ManagedByValue, BackendLabel: "ollama"}
	if instance != "" {
		labels[InstanceLabel] = instance
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": testAPIVersion,
		"kind":       "ModelConfig",
		"metadata": map[string]any{"name": name, "namespace": "kagent",
			"labels":      labels,
			"annotations": map[string]any{ModelAnnotation: model}},
		"spec": map[string]any{"provider": "Ollama", "model": model, "ollama": map[string]any{"host": "http://hand-made:11434"}},
	}}
}

func TestEnsureMarksTheCreatingInstance(t *testing.T) {
	k, client := newFakeKagent(t)
	ctx := context.Background()
	ref, err := k.Ensure(ctx, "smollm2:135m", ollamaEndpoint("smollm2:135m"))
	require.NoError(t, err)
	assert.Equal(t, testInstance, ref.CreatedBy)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "smollm2-135m", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, testInstance, obj.GetLabels()[InstanceLabel], "the creator is written at creation")
}

// An unload removes the ModelConfig the instance created; one another
// model-manager created, or one carrying no instance label (written before
// the label, or by hand with model-manager's labels copied), is left in
// place with its placeholder Secret and named in the answer.
func TestRemoveDeletesOnlyWhatThisInstanceCreated(t *testing.T) {
	for _, tc := range []struct {
		name, createdBy string
		removed         bool
	}{
		{name: "created by this instance", createdBy: testInstance, removed: true},
		{name: "created by another instance", createdBy: "laptop-benchmark"},
		{name: "no instance label", createdBy: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			secret := k0().placeholderSecret("qwen3-0-6b")
			k, client := newFakeKagent(t, managedModelConfig("qwen3-0-6b", "qwen3:0.6b", tc.createdBy), secret)
			ctx := context.Background()

			removal, err := k.Removal(ctx, backend.NameOllama, "qwen3:0.6b")
			require.NoError(t, err)
			err = k.Remove(ctx, backend.NameOllama, "qwen3:0.6b")

			_, getErr := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b", metav1.GetOptions{})
			_, secErr := client.Resource(secretGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b-api-key", metav1.GetOptions{})
			if tc.removed {
				require.NoError(t, err)
				assert.Error(t, getErr, "the ModelConfig is gone")
				assert.Error(t, secErr, "its placeholder Secret went with it")
				assert.Nil(t, removal.Left)
				assert.NotEmpty(t, removal.Objects)
				return
			}
			var left *NotOwnedError
			require.True(t, errors.As(err, &left), "the answer names what was left: %v", err)
			require.ErrorIs(t, err, backend.ErrConflict)
			assert.Equal(t, "kagent", left.Namespace)
			assert.Equal(t, "qwen3-0-6b", left.Name)
			assert.Equal(t, tc.createdBy, left.CreatedBy)
			assert.Contains(t, left.Message, "left in place")
			assert.NoError(t, getErr, "the ModelConfig stays")
			assert.NoError(t, secErr, "its placeholder Secret stays")

			require.NotNil(t, removal.Left, "the dry run says the same")
			assert.Empty(t, removal.Objects, "nothing would be deleted")
		})
	}
}

// k0 is a wirer without a client, for rendering objects.
func k0() *Kagent { return NewKagent(nil, nil, "kagent", DefaultAPIVersion, "", testInstance) }

func TestEnsureRefusesAnotherInstancesModelConfig(t *testing.T) {
	k, client := newFakeKagent(t, managedModelConfig("qwen3-0-6b", "qwen3:0.6b", "laptop-benchmark"))
	ctx := context.Background()

	_, err := k.Ensure(ctx, "qwen3:0.6b", ollamaEndpoint("qwen3:0.6b"))
	var left *NotOwnedError
	require.True(t, errors.As(err, &left), "%v", err)
	require.ErrorIs(t, err, backend.ErrConflict)
	assert.Equal(t, "laptop-benchmark", left.CreatedBy)
	assert.Contains(t, left.Message, "nothing was written")

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b", metav1.GetOptions{})
	require.NoError(t, err)
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "host")
	assert.Equal(t, "http://hand-made:11434", host, "nothing was written")
}

// A ModelConfig without the instance label is adopted by a wire — its spec
// written — but never marked as created here, so an unload still leaves it.
func TestEnsureAdoptsAModelConfigWithoutTheInstanceLabel(t *testing.T) {
	k, client := newFakeKagent(t, managedModelConfig("qwen3-0-6b", "qwen3:0.6b", ""))
	ctx := context.Background()

	ref, err := k.Ensure(ctx, "qwen3:0.6b", ollamaEndpoint("qwen3:0.6b"))
	require.NoError(t, err)
	assert.Equal(t, "qwen3-0-6b", ref.Name)
	assert.Empty(t, ref.CreatedBy)

	obj, err := client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b", metav1.GetOptions{})
	require.NoError(t, err)
	host, _, _ := unstructured.NestedString(obj.Object, "spec", "ollama", "host")
	assert.Equal(t, "http://172.21.0.1:11434", host, "the spec is written")
	assert.NotContains(t, obj.GetLabels(), InstanceLabel, "adopted, not created here")

	var left *NotOwnedError
	require.True(t, errors.As(k.Remove(ctx, backend.NameOllama, "qwen3:0.6b"), &left))
	_, err = client.Resource(testGVR).Namespace("kagent").Get(ctx, "qwen3-0-6b", metav1.GetOptions{})
	assert.NoError(t, err, "the unload leaves it")
}

// A backend-chosen name converges only a ModelConfig this instance created
// (the replacement deletes the old one); any other keeps its name and is
// written in place, never duplicated.
func TestConvergenceLeavesAModelConfigItDidNotCreate(t *testing.T) {
	legacy := managedModelConfig("inferact-qwen3-8-27b-nvfp4", "Inferact/Qwen3.8-27B-NVFP4", "")
	legacy.SetLabels(map[string]string{ManagedByLabel: ManagedByValue, BackendLabel: "kserve"})
	k, client := newFakeKagent(t, legacy)
	ctx := context.Background()

	ep := backend.AgentEndpoint{Backend: backend.NameKServe, Provider: "OpenAI", BaseURL: "http://qwen3-8-27b-predictor.model-serving.svc.cluster.local/v1", Model: "qwen3-8-27b", PlaceholderAPIKey: true, Name: "qwen3-8-27b"}
	ref, err := k.Ensure(ctx, "Inferact/Qwen3.8-27B-NVFP4", ep)
	require.NoError(t, err)
	assert.Equal(t, "inferact-qwen3-8-27b-nvfp4", ref.Name, "kept its name")

	list, err := client.Resource(testGVR).Namespace("kagent").List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, list.Items, 1, "not replaced, not duplicated")
}

func TestInstanceNameIsALabelValue(t *testing.T) {
	assert.Equal(t, "kagent-model-manager", InstanceName("kagent/model-manager"))
	assert.Equal(t, "laptop-local", InstanceName("Laptop.local"))
	long := InstanceName("a-very-long-release-namespace-and-name-that-exceeds-the-label-limit-of-63")
	assert.LessOrEqual(t, len(long), 63)
}
