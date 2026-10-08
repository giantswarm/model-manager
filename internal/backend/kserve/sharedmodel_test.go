package kserve

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
	"github.com/giantswarm/model-manager/internal/wiring"
)

// recordingWirer keeps the endpoint every ModelConfig is written with, and
// renders a ModelConfig carrying it, as the kagent wirer does.
type recordingWirer struct {
	mu   sync.Mutex
	refs map[string]wiring.ModelConfigRef
}

func (w *recordingWirer) ref(model string, ep backend.AgentEndpoint) wiring.ModelConfigRef {
	return wiring.ModelConfigRef{Name: ep.Name, Namespace: "kagent", Backend: ep.Backend, Model: model, Endpoint: ep.BaseURL, ProviderModel: ep.Model, Managed: true}
}

func (w *recordingWirer) Ensure(_ context.Context, model string, ep backend.AgentEndpoint) (*wiring.ModelConfigRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	r := w.ref(model, ep)
	w.refs[ep.Name] = r
	return &r, nil
}

func (w *recordingWirer) Render(_ context.Context, model string, ep backend.AgentEndpoint) (*wiring.Rendered, error) {
	mc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": wiring.DefaultAPIVersion, "kind": "ModelConfig",
		"metadata": map[string]any{"name": ep.Name, "namespace": "kagent"},
		"spec":     map[string]any{"model": ep.Model, "openAI": map[string]any{"baseUrl": ep.BaseURL}},
	}}
	return &wiring.Rendered{Name: ep.Name, Objects: []*unstructured.Unstructured{mc}}, nil
}

func (w *recordingWirer) Remove(context.Context, backend.Name, string) error { return nil }
func (w *recordingWirer) Lookup(context.Context, backend.Name, string) (*wiring.ModelConfigRef, error) {
	return nil, nil
}
func (w *recordingWirer) List(context.Context) ([]wiring.ModelConfigRef, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]wiring.ModelConfigRef, 0, len(w.refs))
	for _, r := range w.refs {
		out = append(out, r)
	}
	return out, nil
}
func (w *recordingWirer) ListAll(ctx context.Context) ([]wiring.ModelConfigRef, error) {
	return w.List(ctx)
}
func (w *recordingWirer) Removal(context.Context, backend.Name, string) (*wiring.Rendered, error) {
	return &wiring.Rendered{}, nil
}
func (w *recordingWirer) Writable(_ context.Context, ep backend.AgentEndpoint) (backend.AgentEndpoint, error) {
	return ep, nil
}

// A model two presets serve is wired to the route of the object the load
// creates — the preset the caller chose — never to an address derived from
// the model id, which no object answers on. The dry run (what mode commit
// lands in git) and the live load (mode apply) wire the same address, and it
// is the endpoint the loaded list reports for the object.
func TestSharedModelIsWiredToTheChosenPresetsRoute(t *testing.T) {
	const alt = "tiny-alt"
	f := newFixture(t, presetConfigMap(alt, presetDoc(alt, tinyRepo, 0.001, "")))
	ctx := context.Background()
	f.setDiscoveryOpts(ctx, discoveryOpts{gateway: "https://models.example.com"})
	w := &recordingWirer{refs: map[string]wiring.ModelConfigRef{}}
	svc := service.New([]backend.Backend{f.b}, jobs.NewManager(), w, &service.WiringInfo{Namespace: "kagent"}, service.Config{AutoWire: true}, nil)
	const route = "https://models.example.com/model-serving/" + alt

	plan, err := svc.PlanLoad(ctx, service.LoadOptions{Backend: string(backend.NameKServe), Preset: alt})
	require.NoError(t, err)
	require.NotEmpty(t, plan.Manifests)
	name, _, _ := unstructured.NestedString(plan.Manifests[0], "metadata", "name")
	assert.Equal(t, alt, name, "the load creates the preset's object")
	require.NotNil(t, plan.Wiring)
	mc := plan.Wiring.Objects()[0]
	assert.Equal(t, alt, mc.GetName())
	baseURL, _, _ := unstructured.NestedString(mc.Object, "spec", "openAI", "baseUrl")
	assert.Equal(t, route+"/v1", baseURL, "the dry run and mode commit name the object's route, not one derived from the model id")

	// The other preset of the model serves already: the load still wires the
	// object it creates.
	require.NoError(t, f.b.Load(ctx, backend.LoadRequest{Name: tinyRepo, Preset: "tiny"}))
	view, err := svc.Load(ctx, service.LoadOptions{Backend: string(backend.NameKServe), Preset: alt})
	require.NoError(t, err)
	require.NotNil(t, view.Wiring)
	require.NotNil(t, view.Wiring.ModelConfig, view.Wiring.Error)
	assert.Equal(t, alt, view.Wiring.ModelConfig.Name)
	assert.Equal(t, route+"/v1", view.Wiring.ModelConfig.Endpoint, "mode apply wires the same address before the model is ready")

	_, err = f.dyn.Resource(llmisvcGVR).Namespace(testServingNS).Get(ctx, alt, metav1.GetOptions{})
	require.NoError(t, err)
	loaded, err := f.b.ListLoaded(ctx)
	require.NoError(t, err)
	for _, l := range loaded {
		if l.Resource == alt {
			assert.Equal(t, route, l.Endpoint, "the served object answers where its ModelConfig points")
		}
	}
}
