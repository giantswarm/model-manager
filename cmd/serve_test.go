package cmd

import (
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/backend/kserve"
)

// remoteKServeDocument is a kserve document whose target is a workload
// cluster, with a CA that parses (the apiserver is never dialled here).
func remoteKServeDocument(t *testing.T) *backend.Document {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	indented := "        " + strings.ReplaceAll(strings.TrimSpace(string(ca)), "\n", "\n        ")
	doc, err := backend.ParseDocument([]byte(`
apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  source: cluster-manager
  kserve:
    target:
      cluster: gpu01
      organization: giantswarm
      apiServer: ` + srv.URL + `
      caBundle: |
` + indented + `
      servingNamespace: model-serving
`))
	require.NoError(t, err)
	return doc
}

// TestBackendBuilderRemoteTarget: a remote target is built only as the caller
// (downstream OAuth) and only with the pod inventory, each refusal naming why.
func TestBackendBuilderRemoteTarget(t *testing.T) {
	backend.Register(backend.NameKServe, kserve.Factory)
	doc := remoteKServeDocument(t)
	base := func(mode string) backend.Options {
		return backend.Options{KServe: backend.KServeOptions{
			Clientset:     kubefake.NewSimpleClientset(),
			Dynamic:       dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
			InventoryMode: mode,
		}}
	}
	log := slog.New(slog.DiscardHandler)

	_, err := backendBuilder(base(kserve.InventoryModePod), false, log)(doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kserve target gpu01: --downstream-oauth is off")
	assert.Contains(t, err.Error(), "every call there would be anonymous")

	_, err = backendBuilder(base(kserve.InventoryModeDaemonSet), true, log)(doc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "inventory mode daemonset dials the cache agents' pod IPs")

	b, err := backendBuilder(base(kserve.InventoryModePod), true, log)(doc)
	require.NoError(t, err)
	assert.Equal(t, &backend.Target{Cluster: "gpu01", Organization: "giantswarm"}, backend.TargetOf(b))
}
