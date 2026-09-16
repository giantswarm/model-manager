package backend

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDocumentHostBackend(t *testing.T) {
	doc, err := ParseDocument([]byte(`
apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
metadata:
  name: ollama
spec:
  kind: ollama
  source: person
  endpoint: http://ollama.local:11434
  agentEndpoint: http://ollama.agents:11434
`))
	require.NoError(t, err)
	assert.Equal(t, NameOllama, doc.Spec.Kind)
	assert.Nil(t, doc.Spec.KServe)

	opts := doc.Options(Options{Ollama: OllamaOptions{Endpoint: "http://static", Timeout: 3}})
	assert.Equal(t, "http://ollama.local:11434", opts.Ollama.Endpoint)
	assert.Equal(t, "http://ollama.agents:11434", opts.Ollama.AgentHost)
	assert.EqualValues(t, 3, opts.Ollama.Timeout, "defaults not named by the document keep the base value")
}

func TestParseDocumentKServeDefaultsAndTarget(t *testing.T) {
	doc, err := ParseDocument([]byte(`
apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  source: cluster-manager
  kserve:
    target:
      cluster: gpu01
      organization: giantswarm
      apiServer: https://api.gpu01.example:6443
      caBundle: |
        -----BEGIN CERTIFICATE-----
        MIIB
        -----END CERTIFICATE-----
      servingNamespace: model-serving
    router:
      scheduler: true
`))
	require.NoError(t, err)
	assert.Equal(t, "agent-platform-model-serving", doc.Spec.KServe.Discovery.Name, "discovery name default")
	assert.Equal(t, "model-serving", doc.Spec.KServe.Discovery.Namespace, "discovery namespace defaults to the serving namespace")
	assert.False(t, doc.Spec.KServe.Target.Local())
	assert.Equal(t, &Target{Cluster: "gpu01", Organization: "giantswarm"}, doc.Spec.KServe.Target.Identity(), "the identity never carries the apiserver")

	opts := doc.Options(Options{KServe: KServeOptions{Namespace: "static-ns", DownloadImage: "img"}})
	assert.Equal(t, "model-serving", opts.KServe.Namespace)
	assert.Equal(t, "model-serving", opts.KServe.DiscoveryNamespace)
	assert.Equal(t, "gpu01", opts.KServe.Target.Cluster)
	assert.Equal(t, "img", opts.KServe.DownloadImage)
	assert.True(t, opts.KServe.Router.Scheduler, "the document's router shape lands in the options")

	local := NewDocument(DocumentSpec{Kind: NameKServe, KServe: &KServeSpec{Target: Target{ServingNamespace: "ns"}}})
	require.NoError(t, local.Validate())
	assert.Equal(t, TargetLocal, local.Spec.KServe.Target.Cluster)
	assert.Nil(t, local.Spec.KServe.Target.Identity(), "local has no target identity")
	assert.Equal(t, SourcePerson, local.Spec.Source)
	assert.False(t, local.Options(Options{}).KServe.Router.Scheduler, "no router block: the route alone")
}

func TestParseDocumentNamesTheFailingField(t *testing.T) {
	cases := []struct{ want, raw string }{ // test fixtures
		{"spec.kind: required", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {source: person}\n"},
		{"spec.kind: unknown \"vllm\"", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: vllm, source: person}\n"},
		{"kind: must be ModelBackend", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ServingPreset\nspec: {kind: ollama}\n"},
		{"apiVersion: must be", "apiVersion: v1\nkind: ModelBackend\nspec: {kind: ollama}\n"},
		{"spec.source: must be person or cluster-manager", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama, source: static, endpoint: http://x}\n"},
		{"spec.endpoint: required for ollama", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama}\n"},
		{"spec.endpoint: must be an http(s) URL", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: lmstudio, endpoint: lmstudio:1234}\n"},
		{"spec.kserve: not accepted for lemonade", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: lemonade, endpoint: http://x, kserve: {target: {servingNamespace: a}}}\n"},
		{"spec.credentials: not supported by ollama", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama, endpoint: http://x, credentials: {secretRef: {name: s}}}\n"},
		{"spec.kserve: required for kserve", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: kserve}\n"},
		{"spec.kserve.target.servingNamespace: required", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: kserve, kserve: {target: {cluster: local}}}\n"},
		{"spec.kserve.target.apiServer: required", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: kserve, kserve: {target: {cluster: gpu01, servingNamespace: a}}}\n"},
		{"spec.kserve.target.caBundle: required", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: kserve, kserve: {target: {cluster: gpu01, apiServer: 'https://a:6443', servingNamespace: a}}}\n"},
		{"spec.kserve.target.apiServer: not accepted for the local cluster", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: kserve, kserve: {target: {apiServer: 'https://a', servingNamespace: a}}}\n"},
		{"metadata.name: must equal spec.kind", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nmetadata: {name: other}\nspec: {kind: ollama, endpoint: http://x}\n"},
		{"unknown field", "apiVersion: agent-platform.giantswarm.io/v1alpha1\nkind: ModelBackend\nspec: {kind: ollama, endpoint: http://x, endpoints: [a]}\n"},
	}
	for _, c := range cases {
		want, raw := c.want, c.raw
		_, err := ParseDocument([]byte(raw))
		require.Error(t, err, want)
		assert.Contains(t, err.Error(), want)
	}
}

func TestDocumentRenderRoundTripAndConfigMap(t *testing.T) {
	doc := NewDocument(DocumentSpec{Kind: NameLemonade, Endpoint: "http://lemonade:13305"})
	raw, err := doc.Render()
	require.NoError(t, err)
	again, err := ParseDocument(raw)
	require.NoError(t, err)
	assert.Equal(t, doc, again)

	cm, err := doc.ConfigMap("agent-platform")
	require.NoError(t, err)
	assert.Equal(t, "model-backend-lemonade", cm.Name)
	assert.Equal(t, "agent-platform", cm.Namespace)
	assert.Equal(t, "true", cm.Labels[DocumentLabel])
	assert.Equal(t, SourcePerson, cm.Labels[DocumentSourceLabel])
	assert.True(t, strings.Contains(cm.Data[DocumentKey], "kind: lemonade"))
}
