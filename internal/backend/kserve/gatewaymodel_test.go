package kserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
)

const testLLMEndpoint = "http://agentgateway.agent-platform.svc:8081"

// withLLMEndpoint publishes spec.llmEndpoint in discovery.
func (f *fixture) withLLMEndpoint(ctx context.Context) {
	f.t.Helper()
	f.setDiscoveryOpts(ctx, discoveryOpts{llmEndpoint: testLLMEndpoint})
}

// gatewayModel reads the driver's AgentgatewayModel of a public name; nil when
// there is none.
func (f *fixture) gatewayModel(ctx context.Context, name string) *unstructured.Unstructured {
	f.t.Helper()
	obj, err := f.dyn.Resource(agentgatewayModelGVR).Namespace(testPlatformNS).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(f.t, err)
	return obj
}

// A Ready model created from a preset is put on the LLM endpoint under the
// preset's name: an Internal concrete model — a Custom provider on the
// workload Service's /v1 with the formats its server registered, matched on
// the Hugging Face id vLLM serves — and the public virtual model targeting
// it, which rewrites the request's model; list_loaded_models names the public
// name and the endpoint.
func TestReadyModelIsPutOnTheLLMEndpoint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.withLLMEndpoint(ctx)
	f.serve("tiny", vllmServer(t, devVersion, generateDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Equal(t, "tiny", lm.PublicName)
	assert.Equal(t, testLLMEndpoint, lm.Endpoint, "the endpoint a client reaches the public name at")
	assert.Empty(t, lm.PublicNameReason)

	parents := []any{map[string]any{"group": "gateway.networking.k8s.io", "kind": "HTTPRoute", "name": "agent-platform-connectivity-llm", "namespace": testPlatformNS}}
	public := f.gatewayModel(ctx, "tiny")
	require.NotNil(t, public, "the public virtual model exists")
	assert.Equal(t, map[string]string{ManagedByLabel: ManagedByValue, BackendLabel: "kserve", PresetLabel: "tiny"}, public.GetLabels())
	assert.Equal(t, testServingNS+"/tiny", public.GetAnnotations()[servingAnnotation])
	assert.Equal(t, map[string]any{
		"parentRefs":   parents,
		"virtualModel": map[string]any{"weighted": map[string]any{"targets": []any{map[string]any{"modelRef": map[string]any{"name": "tiny-workload"}}}}},
	}, public.Object["spec"], "the public name rewrites the request's model to its target's")

	concrete := f.gatewayModel(ctx, "tiny-workload")
	require.NotNil(t, concrete, "the concrete model exists")
	assert.Equal(t, "tiny", concrete.GetLabels()[PresetLabel])
	assert.Equal(t, map[string]any{
		"parentRefs": parents,
		"match":      map[string]any{"model": tinyRepo},
		"visibility": "Internal",
		"provider":   "Custom",
		"baseURL":    workloadURL("tiny", testServingNS) + "/v1",
		"custom": map[string]any{"formats": []any{
			map[string]any{"type": backend.InterfaceCompletions},
			map[string]any{"type": backend.InterfaceResponses},
			map[string]any{"type": backend.InterfaceMessages},
			map[string]any{"type": backend.InterfaceAnthropicTokenCount},
		}},
	}, concrete.Object["spec"], "Internal, matched on the id vLLM serves, the formats exactly the interfaces the model reports, /v1 on the baseURL")

	// The agents ride the endpoint under the public name, keyless.
	ep := f.b.AgentEndpoint(tinyRepo)
	assert.Equal(t, backend.AgentEndpoint{Provider: "OpenAI", BaseURL: testLLMEndpoint + "/v1", Model: "tiny", PlaceholderAPIKey: true, Name: "tiny"}, ep)
}

// Unload takes the model off the endpoint in the same call.
func TestUnloadTakesTheModelOffTheLLMEndpoint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.withLLMEndpoint(ctx)
	f.serve("tiny", vllmServer(t, devVersion, generateDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now())
	loadedOne(t, f.b)
	require.NotNil(t, f.gatewayModel(ctx, "tiny"))

	require.NoError(t, f.b.Unload(ctx, "tiny"))
	assert.Nil(t, f.gatewayModel(ctx, "tiny"), "gone with the serving object")
	assert.Nil(t, f.gatewayModel(ctx, "tiny-workload"), "the concrete model too")
}

// A model whose server reports no interfaces gets no object, and the list
// says why; one not Ready yet neither.
func TestModelWithoutInterfacesStaysOffTheLLMEndpoint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.withLLMEndpoint(ctx)
	f.serve("tiny", &modelServer{version: devVersion})
	f.pendingLLMISVC(ctx, "tiny")

	lm := loadedOne(t, f.b)
	assert.Empty(t, lm.PublicName)
	assert.Equal(t, "not on the LLM endpoint until the model is Ready", lm.PublicNameReason)

	f.setReady(ctx, "tiny", time.Now())
	lm = loadedOne(t, f.b)
	assert.Empty(t, lm.PublicName)
	assert.Contains(t, lm.PublicNameReason, "the server reports no API interfaces")
	assert.Contains(t, lm.PublicNameReason, "--disable-fastapi-docs")
	assert.Nil(t, f.gatewayModel(ctx, "tiny"))
	assert.Equal(t, workloadURL("tiny", testServingNS), lm.Endpoint, "the model's own address while it is not on the endpoint")
}

// A model restarting (Ready → not Ready) keeps its object; the object of a
// serving object deleted elsewhere goes; one edited by hand is rewritten at
// the next resync, one deleted by hand comes back.
func TestGatewayModelsFollowTheServingObjects(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.withLLMEndpoint(ctx)
	f.serve("tiny", vllmServer(t, devVersion, generateDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now().Add(-time.Hour))
	loadedOne(t, f.b)
	require.NotNil(t, f.gatewayModel(ctx, "tiny"))

	llmisvcs := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS)
	obj, err := llmisvcs.Get(ctx, "tiny", metav1.GetOptions{})
	require.NoError(t, err)
	obj.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "False", "reason": "WorkloadsNotReady"}}}
	_, err = llmisvcs.Update(ctx, obj, metav1.UpdateOptions{})
	require.NoError(t, err)
	lm := loadedOne(t, f.b)
	assert.NotNil(t, f.gatewayModel(ctx, "tiny"), "a restarting model keeps its place on the endpoint")
	assert.Equal(t, "tiny", lm.PublicName)

	f.setReady(ctx, "tiny", time.Now())
	loadedOne(t, f.b)
	require.NoError(t, f.dyn.Resource(agentgatewayModelGVR).Namespace(testPlatformNS).Delete(ctx, "tiny-workload", metav1.DeleteOptions{}))
	f.b.gwMu.Lock()
	f.b.gwSyncedAt = time.Now().Add(-gatewayResync)
	f.b.gwMu.Unlock()
	loadedOne(t, f.b)
	assert.NotNil(t, f.gatewayModel(ctx, "tiny-workload"), "an object deleted by hand is back after the resync")

	require.NoError(t, llmisvcs.Delete(ctx, "tiny", metav1.DeleteOptions{}))
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	assert.Empty(t, loaded)
	assert.Nil(t, f.gatewayModel(ctx, "tiny"), "the objects of a serving object deleted elsewhere go")
	assert.Nil(t, f.gatewayModel(ctx, "tiny-workload"))
}

// A hand-written LLMInferenceService has no preset and so no public name.
func TestForeignServingObjectIsNotPutOnTheLLMEndpoint(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.withLLMEndpoint(ctx)
	obj := llmisvcObject("embed", "hf://org/embed", map[string]string{ManagedByLabel: "kustomize"})
	obj.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Ready", "status": "True"}}}
	_, err := f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Create(ctx, obj, metav1.CreateOptions{})
	require.NoError(t, err)
	f.serve("embed", vllmServer(t, devVersion, poolingDoc))

	lm := loadedOne(t, f.b)
	assert.Empty(t, lm.PublicName)
	assert.Contains(t, lm.PublicNameReason, "not created from a serving preset")
	assert.Len(t, lm.Interfaces, 1, "its interfaces are reported all the same")
	assert.Nil(t, f.gatewayModel(ctx, "embed"))
}

// Without spec.llmEndpoint nothing changes: no object, the model's own
// address, the agents on it as before.
func TestWithoutLLMEndpointNothingIsWritten(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	f.serve("tiny", vllmServer(t, devVersion, generateDoc))
	f.readyLLMISVC(ctx, "tiny", time.Now())

	lm := loadedOne(t, f.b)
	assert.Empty(t, lm.PublicName)
	assert.Empty(t, lm.PublicNameReason)
	list, err := f.dyn.Resource(agentgatewayModelGVR).Namespace(testPlatformNS).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, list.Items)
	assert.Equal(t, workloadURL("tiny", testServingNS)+"/v1", f.b.AgentEndpoint(tinyRepo).BaseURL)
}

func TestNewLLMEndpoint(t *testing.T) {
	ep, err := newLLMEndpoint([]map[string]any{{"kind": "HTTPRoute", "name": "llm"}}, "http://gw:8081/", "", "platform")
	require.NoError(t, err)
	assert.Equal(t, "platform", ep.Namespace, "a parent without a namespace is in discovery's")
	assert.Equal(t, "http://gw:8081", ep.Endpoint)
	assert.Equal(t, "http://gw:8081", ep.clientURL(), "the listener while nothing is published")
	ep, err = newLLMEndpoint([]map[string]any{{"name": "llm"}}, "http://gw:8081", "https://llm.example.test/", "platform")
	require.NoError(t, err)
	assert.Equal(t, "https://llm.example.test", ep.clientURL(), "a client is given the public URL")
	_, err = newLLMEndpoint(nil, "http://gw:8081", "", "platform")
	assert.Error(t, err)
	_, err = newLLMEndpoint([]map[string]any{{"name": "a", "namespace": "x"}, {"name": "b", "namespace": "y"}}, "http://gw:8081", "", "platform")
	assert.ErrorContains(t, err, "its own namespace")
}
