package api

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/registry"
	"github.com/giantswarm/model-manager/internal/service"
)

const testNamespace = "agent-platform"

// The ollama documents the registry tests move between: valid, failing the
// schema (no endpoint; the kind is never known), and valid but refused by the
// fixture's builder (unbuildableEndpoint), a broken document of the same kind.
const (
	unbuildableEndpoint = "http://unbuildable:11434"
	goodOllamaDocument  = "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama, endpoint: http://ollama:11434}\n"
	badOllamaDocument   = "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama}\n"
	unbuildableDocument = "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama, endpoint: " + unbuildableEndpoint + "}\n"
)

// registrationFixture runs the real Registry over a fake clientset with the
// MCP tools writing through a Store — the add_backend → watch → list_backends
// chain, minus the apiserver.
type registrationFixture struct {
	client *kubefake.Clientset
	svc    *service.Service
	wirer  *fakeWirer
	srv    *mcpserver.MCPServer
	cancel context.CancelFunc
}

func newRegistrationFixture(t *testing.T, static ...backend.Backend) *registrationFixture {
	t.Helper()
	return newRegistrationFixtureBuilding(t, func(doc *backend.Document) (backend.Backend, error) {
		if doc.Spec.Endpoint == unbuildableEndpoint {
			return nil, errors.New("unbuildable endpoint")
		}
		fb := newFakeBackend()
		fb.name = doc.Spec.Kind
		return fb, nil
	}, static...)
}

// newRegistrationFixtureBuilding is newRegistrationFixture with the builder
// the registry turns a document into a backend with.
func newRegistrationFixtureBuilding(t *testing.T, build registry.Builder, static ...backend.Backend) *registrationFixture {
	t.Helper()
	client := kubefake.NewSimpleClientset()
	fw := newFakeWirer()
	svc := service.New(static, jobs.NewManager(), fw, &service.WiringInfo{Namespace: "kagent"}, service.Config{}, nil)
	reg := registry.New(client, testNamespace, build, svc, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = reg.Run(ctx) }()
	store := registry.NewStore(func(context.Context) kubernetes.Interface { return client }, testNamespace)
	srv := NewMCPServer(svc, buildinfo.Info{Version: "test"}, WithBackendStore(store))
	t.Cleanup(cancel)
	return &registrationFixture{client: client, svc: svc, wirer: fw, srv: srv, cancel: cancel}
}

func (f *registrationFixture) backends(t *testing.T) map[string]any {
	t.Helper()
	text, isErr := callTool(t, f.srv, ToolListBackends, nil)
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	return out
}

func TestZeroBackends(t *testing.T) {
	f := newRegistrationFixture(t)

	out := f.backends(t)
	assert.Empty(t, out["backends"], "list_backends is empty with no backend")
	assert.Nil(t, out["invalid"])

	for _, tool := range []string{ToolGetBackend, ToolListModels, ToolListLoadedModels} {
		text, isErr := callTool(t, f.srv, tool, nil)
		assert.True(t, isErr, tool)
		assert.Contains(t, text, "no_backend: no backend registered: register one with add_backend", tool)
	}
	text, isErr := callTool(t, f.srv, ToolPullModel, map[string]any{argModel: "x"})
	assert.True(t, isErr)
	assert.Contains(t, text, "no backend registered")
}

func TestAddBackendDryRunAndApply(t *testing.T) {
	f := newRegistrationFixture(t)

	// Dry run renders the document and writes nothing.
	text, isErr := callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: "http://ollama:11434", argDryRun: true})
	require.False(t, isErr, text)
	var dry map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dry))
	assert.Equal(t, true, dry["dryRun"])
	assert.Contains(t, dry["document"], "kind: ollama")
	assert.Contains(t, dry["document"], "source: person")
	cm := dry["configMap"].(map[string]any)
	assert.Equal(t, "model-backend-ollama", cm["name"])
	assert.Equal(t, testNamespace, cm["namespace"])
	_, err := f.client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), "model-backend-ollama", metav1.GetOptions{})
	assert.Error(t, err, "dry run must not write")

	// Apply writes the ConfigMap; the watch registers the backend.
	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: "http://ollama:11434"})
	require.False(t, isErr, text)
	var applied map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &applied))
	assert.Equal(t, true, applied["created"])
	assert.Equal(t, true, applied["registered"])
	got, err := f.client.CoreV1().ConfigMaps(testNamespace).Get(context.Background(), "model-backend-ollama", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "true", got.Labels[backend.DocumentLabel])
	assert.Equal(t, "person", got.Labels[backend.DocumentSourceLabel])

	out := f.backends(t)
	list := out["backends"].([]any)
	require.Len(t, list, 1)
	assert.Equal(t, "ollama", list[0].(map[string]any)["backend"])
	assert.Equal(t, "person", list[0].(map[string]any)["source"])

	// A second apply of the same kind replaces the document (one per kind).
	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: "http://other:11434", argSource: "cluster-manager"})
	require.False(t, isErr, text)
	require.NoError(t, json.Unmarshal([]byte(text), &applied))
	assert.Equal(t, false, applied["created"])
	require.Eventually(t, func() bool { return f.svc.Source("ollama") == "cluster-manager" }, 5*time.Second, 20*time.Millisecond)

	// Registered backends answer backend-scoped tools.
	text, isErr = callTool(t, f.srv, ToolGetBackend, nil)
	require.False(t, isErr, text)
	assert.Contains(t, text, `"source": "cluster-manager"`)
}

// TestAddBackendNamesAReadTimeRefusal: a document the registry refuses to
// build (a remote kserve target without downstream OAuth) is answered with
// the reason, not a bare registered: false; a registered document edited
// into the refusal drops its backend in the same step that reports it.
func TestAddBackendNamesAReadTimeRefusal(t *testing.T) {
	const refusedEndpoint = "http://remote-target:11434"
	const refusal = "build ollama backend: kserve target wc1: --downstream-oauth is off"
	f := newRegistrationFixtureBuilding(t, func(doc *backend.Document) (backend.Backend, error) {
		if doc.Spec.Endpoint == refusedEndpoint {
			return nil, errors.New("kserve target wc1: --downstream-oauth is off")
		}
		fb := newFakeBackend()
		fb.name = doc.Spec.Kind
		return fb, nil
	})

	// A new document: not loaded, and the answer names the refusal.
	text, isErr := callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: refusedEndpoint})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, false, out["registered"])
	assert.Equal(t, refusal, out["error"])

	// Registered, then edited into the refusal: dropped and reported.
	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: "http://ollama:11434"})
	require.False(t, isErr, text)
	out = nil
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	require.Equal(t, true, out["registered"])
	assert.Nil(t, out["error"])

	stop := sampleRegisteredAndReported(t, f.svc)
	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: refusedEndpoint})
	require.False(t, isErr, text)
	require.Eventually(t, func() bool {
		_, has := f.svc.Has(backend.NameOllama)
		return !has
	}, 5*time.Second, 20*time.Millisecond, "the refused edit drops the registered backend")
	stop()
	invalid := f.backends(t)["invalid"].([]any)
	require.Len(t, invalid, 1)
	assert.Equal(t, "model-backend-ollama", invalid[0].(map[string]any)["configMap"])
	assert.Equal(t, refusal, invalid[0].(map[string]any)["error"])
}

func TestAddBackendRefusals(t *testing.T) {
	static := newFakeBackend()
	static.name = backend.NameLemonade
	f := newRegistrationFixture(t, static)

	text, isErr := callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: "http://x", argMode: "commit"})
	assert.True(t, isErr)
	assert.Contains(t, text, "unsupported: operation not supported: mode commit")
	assert.Contains(t, text, "use mode apply")

	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "ollama"})
	assert.True(t, isErr)
	assert.Contains(t, text, "invalid_request: invalid request: spec.endpoint: required for ollama")

	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "kserve", argCluster: "gpu01", argServingNamespace: "ns"})
	assert.True(t, isErr)
	assert.Contains(t, text, "spec.kserve.target.apiServer: required")

	text, isErr = callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "lemonade", argEndpoint: "http://x"})
	assert.True(t, isErr)
	assert.Contains(t, text, "conflict: backend lemonade is configured statically")

	text, isErr = callTool(t, f.srv, ToolRemoveBackend, map[string]any{argKind: "lemonade"})
	assert.True(t, isErr)
	assert.Contains(t, text, "conflict: backend lemonade is configured statically")

	out := f.backends(t)
	list := out["backends"].([]any)
	require.Len(t, list, 1)
	assert.Equal(t, "static", list[0].(map[string]any)["source"])
}

func TestRemoveBackendDropsModelConfigs(t *testing.T) {
	f := newRegistrationFixture(t)
	text, isErr := callTool(t, f.srv, ToolAddBackend, map[string]any{argKind: "kserve", argServingNamespace: "model-serving"})
	require.False(t, isErr, text)
	_, err := f.wirer.Ensure(context.Background(), "org/model", backend.AgentEndpoint{Backend: backend.NameKServe, BaseURL: "http://m/v1"})
	require.NoError(t, err)

	text, isErr = callTool(t, f.srv, ToolRemoveBackend, map[string]any{argKind: "kserve", argDryRun: true})
	require.False(t, isErr, text)
	var dry map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &dry))
	assert.Equal(t, []any{"org/model"}, dry["modelConfigs"])
	_, ok := f.svc.Has(backend.NameKServe)
	assert.True(t, ok, "dry run keeps the backend")

	text, isErr = callTool(t, f.srv, ToolRemoveBackend, map[string]any{argKind: "kserve"})
	require.False(t, isErr, text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	assert.Equal(t, []any{"org/model"}, out["unwired"])
	assert.Equal(t, true, out["removed"])
	assert.Equal(t, true, out["deregistered"])
	refs, _ := f.wirer.List(context.Background())
	assert.Empty(t, refs)
	assert.Empty(t, f.backends(t)["backends"])

	text, isErr = callTool(t, f.srv, ToolRemoveBackend, map[string]any{argKind: "kserve"})
	assert.True(t, isErr)
	assert.Contains(t, text, "not_found")
}

func TestInvalidDocumentIsReportedNotLoaded(t *testing.T) {
	f := newRegistrationFixture(t)
	bad := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "model-backend-ollama", Namespace: testNamespace, Labels: map[string]string{backend.DocumentLabel: "true"}},
		Data:       map[string]string{backend.DocumentKey: badOllamaDocument},
	}
	_, err := f.client.CoreV1().ConfigMaps(testNamespace).Create(context.Background(), bad, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(f.svc.InvalidDocuments()) == 1 }, 5*time.Second, 20*time.Millisecond)

	out := f.backends(t)
	assert.Empty(t, out["backends"])
	invalid := out["invalid"].([]any)[0].(map[string]any)
	assert.Equal(t, "model-backend-ollama", invalid["configMap"])
	assert.Equal(t, "spec.endpoint: required for ollama", invalid["error"])

	// Fixing the document loads it and clears the report.
	bad.Data[backend.DocumentKey] = goodOllamaDocument
	_, err = f.client.CoreV1().ConfigMaps(testNamespace).Update(context.Background(), bad, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return ok }, 5*time.Second, 20*time.Millisecond)
	assert.Empty(t, f.svc.InvalidDocuments())

	// Breaking the loaded document drops its backend, also when the document
	// still names ollama: the last good backend is not left serving beside
	// the report.
	bad.Data[backend.DocumentKey] = unbuildableDocument
	_, err = f.client.CoreV1().ConfigMaps(testNamespace).Update(context.Background(), bad, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(f.svc.InvalidDocuments()) == 1 }, 5*time.Second, 20*time.Millisecond)
	out = f.backends(t)
	assert.Empty(t, out["backends"])
	assert.Equal(t, "build ollama backend: unbuildable endpoint", out["invalid"].([]any)[0].(map[string]any)["error"])

	bad.Data[backend.DocumentKey] = goodOllamaDocument
	_, err = f.client.CoreV1().ConfigMaps(testNamespace).Update(context.Background(), bad, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return ok }, 5*time.Second, 20*time.Millisecond)
	assert.Empty(t, f.svc.InvalidDocuments())

	// A ConfigMap named for another kind is refused by name.
	misnamed := bad.DeepCopy()
	misnamed.Name = "model-backend-lmstudio"
	misnamed.ResourceVersion = ""
	_, err = f.client.CoreV1().ConfigMaps(testNamespace).Create(context.Background(), misnamed, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { return len(f.svc.InvalidDocuments()) == 1 }, 5*time.Second, 20*time.Millisecond)
	assert.Contains(t, f.svc.InvalidDocuments()[0].Error, "metadata.name: the ConfigMap of a ollama document is named model-backend-ollama")

	// Deleting the document drops the backend.
	require.NoError(t, f.client.CoreV1().ConfigMaps(testNamespace).Delete(context.Background(), "model-backend-ollama", metav1.DeleteOptions{}))
	require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return !ok }, 5*time.Second, 20*time.Millisecond)
}

func TestNoStoreRefusesRegistration(t *testing.T) {
	svc := service.New(nil, jobs.NewManager(), nil, nil, service.Config{}, nil)
	srv := NewMCPServer(svc, buildinfo.Info{Version: "test"})
	text, isErr := callTool(t, srv, ToolAddBackend, map[string]any{argKind: "ollama", argEndpoint: "http://x"})
	assert.True(t, isErr)
	assert.Contains(t, text, "backend registration needs Kubernetes access")
}

// A fixed document is registered and its report cleared in one step: no
// reader sees the backend and its stale `invalid` entry at once
// (giantswarm/model-manager#101).
func TestRegisterDocumentClearsTheReportAtomically(t *testing.T) {
	static := newFakeBackend()
	static.name = backend.NameLMStudio
	svc := service.New([]backend.Backend{static}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	svc.DeregisterDocument("", "model-backend-ollama", "spec.endpoint: required for ollama")
	svc.DeregisterDocument("", "model-backend-lmstudio", "spec.endpoint: required for lmstudio")

	ollama := newFakeBackend()
	ollama.name = backend.NameOllama
	require.NoError(t, svc.RegisterDocument(ollama, backend.SourcePerson, "model-backend-ollama"))
	_, has := svc.Has(backend.NameOllama)
	assert.True(t, has)
	assert.Equal(t, []service.InvalidDocument{{ConfigMap: "model-backend-lmstudio", Error: "spec.endpoint: required for lmstudio"}}, svc.InvalidDocuments(), "the registered document's report is gone, the other stays")

	// A refused registration (the kind is static) leaves the report to the caller.
	lm := newFakeBackend()
	lm.name = backend.NameLMStudio
	err := svc.RegisterDocument(lm, backend.SourcePerson, "model-backend-lmstudio")
	require.ErrorIs(t, err, service.ErrStaticBackend)
	assert.Len(t, svc.InvalidDocuments(), 1, "the refused document stays reported")
}

// The mirror: a broken document is reported and its backend dropped in one
// step, so no reader sees the backend and the fresh report at once.
func TestDeregisterDocumentReportsAtomically(t *testing.T) {
	static := newFakeBackend()
	static.name = backend.NameLMStudio
	svc := service.New([]backend.Backend{static}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	ollama := newFakeBackend()
	ollama.name = backend.NameOllama
	require.NoError(t, svc.RegisterDocument(ollama, backend.SourcePerson, "model-backend-ollama"))

	assert.True(t, svc.DeregisterDocument(backend.NameOllama, "model-backend-ollama", "spec.endpoint: required for ollama"))
	_, has := svc.Has(backend.NameOllama)
	assert.False(t, has)
	assert.Equal(t, []service.InvalidDocument{{ConfigMap: "model-backend-ollama", Error: "spec.endpoint: required for ollama"}}, svc.InvalidDocuments())

	// A static kind is never dropped; the report stands.
	assert.False(t, svc.DeregisterDocument(backend.NameLMStudio, "model-backend-lmstudio", "spec.endpoint: required for lmstudio"))
	_, has = svc.Has(backend.NameLMStudio)
	assert.True(t, has)
	assert.Len(t, svc.InvalidDocuments(), 2)

	// An empty problem clears the report (the document was deleted).
	assert.False(t, svc.DeregisterDocument(backend.NameOllama, "model-backend-ollama", ""))
	assert.Equal(t, []service.InvalidDocument{{ConfigMap: "model-backend-lmstudio", Error: "spec.endpoint: required for lmstudio"}}, svc.InvalidDocuments())
}

// sampleRegisteredAndReported starts a sampler that reads what list_backends
// answers, at one moment, until the returned stop is called; stop fails the
// test if the sampler ever saw the ollama backend registered while its own
// document was reported. Read one after the other, the two facts could pair
// one event's registration with a later one's report, a state the service
// never held.
func sampleRegisteredAndReported(t *testing.T, svc *service.Service) (stop func()) {
	t.Helper()
	ctx := context.Background()
	done := make(chan struct{})
	finished := make(chan struct{})
	violations := make(chan string, 1)
	go func() {
		defer close(finished)
		for {
			select {
			case <-done:
				return
			default:
			}
			backends, invalid := svc.Backends(ctx)
			if !slices.ContainsFunc(backends, func(b service.BackendResponse) bool { return b.Backend == backend.NameOllama }) {
				continue
			}
			for _, d := range invalid {
				if d.ConfigMap == "model-backend-ollama" {
					select {
					case violations <- d.Error:
					default:
					}
				}
			}
		}
	}()
	return func() {
		t.Helper()
		close(done)
		<-finished
		select {
		case v := <-violations:
			t.Fatalf("the ollama backend was registered while its document was reported: %s", v)
		default:
		}
	}
}

// The invariant through the real registry, on the fix: a document that goes
// invalid → fixed → deleted repeatedly is never seen registered with its old
// report standing (giantswarm/model-manager#101).
func TestFixedDocumentIsNeverRegisteredAndInvalidAtOnce(t *testing.T) {
	f := newRegistrationFixture(t)
	ctx := context.Background()
	stop := sampleRegisteredAndReported(t, f.svc)
	cms := f.client.CoreV1().ConfigMaps(testNamespace)
	for i := 0; i < 25; i++ {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "model-backend-ollama", Namespace: testNamespace, Labels: map[string]string{backend.DocumentLabel: "true"}},
			Data:       map[string]string{backend.DocumentKey: badOllamaDocument},
		}
		created, err := cms.Create(ctx, cm, metav1.CreateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { return len(f.svc.InvalidDocuments()) == 1 }, 5*time.Second, time.Millisecond)
		created.Data[backend.DocumentKey] = goodOllamaDocument
		_, err = cms.Update(ctx, created, metav1.UpdateOptions{})
		require.NoError(t, err)
		require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return ok }, 5*time.Second, time.Millisecond)
		assert.Empty(t, f.svc.InvalidDocuments(), "registered implies not reported")
		require.NoError(t, cms.Delete(ctx, "model-backend-ollama", metav1.DeleteOptions{}))
		require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return !ok }, 5*time.Second, time.Millisecond)
	}
	stop()
}

// The invariant through the real registry, on the break: a registered
// document updated good → bad and back repeatedly — failing the schema, and
// failing its build while it still names ollama — is never seen registered
// with its new report standing, and once reported its backend is gone.
func TestBrokenDocumentIsNeverRegisteredAndInvalidAtOnce(t *testing.T) {
	f := newRegistrationFixture(t)
	ctx := context.Background()
	stop := sampleRegisteredAndReported(t, f.svc)
	cms := f.client.CoreV1().ConfigMaps(testNamespace)
	cm, err := cms.Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "model-backend-ollama", Namespace: testNamespace, Labels: map[string]string{backend.DocumentLabel: "true"}},
		Data:       map[string]string{backend.DocumentKey: goodOllamaDocument},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return ok }, 5*time.Second, time.Millisecond)
	for i := 0; i < 25; i++ {
		for _, broken := range []string{badOllamaDocument, unbuildableDocument} {
			cm.Data[backend.DocumentKey] = broken
			cm, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Eventually(t, func() bool { return len(f.svc.InvalidDocuments()) == 1 }, 5*time.Second, time.Millisecond)
			_, has := f.svc.Has(backend.NameOllama)
			assert.False(t, has, "reported implies not registered")
			cm.Data[backend.DocumentKey] = goodOllamaDocument
			cm, err = cms.Update(ctx, cm, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Eventually(t, func() bool { _, ok := f.svc.Has(backend.NameOllama); return ok }, 5*time.Second, time.Millisecond)
			assert.Empty(t, f.svc.InvalidDocuments(), "registered implies not reported")
		}
	}
	stop()
}
