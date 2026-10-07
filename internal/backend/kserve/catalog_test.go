package kserve

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/giantswarm/model-manager/internal/backend"
)

// newCatalog is a Catalog over a fake local cluster holding objs: no backend,
// no dynamic client, the discovery namespace alone naming where the presets
// are, as the serve command builds it from the static options.
func newCatalog(t *testing.T, objs ...runtime.Object) *Catalog {
	t.Helper()
	c, err := NewCatalog(backend.KServeOptions{Clientset: kubefake.NewSimpleClientset(objs...), DiscoveryNamespace: testPlatformNS})
	require.NoError(t, err)
	return c
}

func TestCatalogNeedsKubernetesAccess(t *testing.T) {
	_, err := NewCatalog(backend.KServeOptions{})
	require.Error(t, err)
}

// A preset named without a backend is answered from its declaration: the
// numbers a judged answer carries, verdict unverified, nothing judged.
func TestCatalogFitCheckAnswersThePresetDeclaration(t *testing.T) {
	c := newCatalog(t,
		presetConfigMap("big", presetDoc("big", bigRepo, 100, "    minComputeCapability: \"8.9\"\n")),
		presetConfigMap("tiny", presetDoc("tiny", tinyRepo, 0.001, "")),
	)
	res, err := c.FitCheck(context.Background(), backend.FitRequest{Preset: "big"})
	require.NoError(t, err)
	assert.Equal(t, backend.VerdictUnverified, res.Verdict)
	assert.False(t, res.Fits)
	assert.True(t, res.Retryable)
	assert.Equal(t, bigRepo, res.Model)
	assert.Equal(t, "big", res.Preset)
	assert.Equal(t, []string{"big"}, res.Presets)
	assert.Equal(t, int64(100*gib), res.WeightsBytes)
	assert.Equal(t, int64(100*gib), res.DeclaredWeightsBytes)
	assert.Equal(t, weightsSourcePreset, res.WeightsSource)
	assert.Equal(t, int64(gib), res.OverheadBytes)
	assert.Equal(t, int64(101*gib), res.RequiredBytes)
	assert.Equal(t, "8.9", res.ComputeCapabilityRequired)
	assert.Equal(t, int64(1), res.DevicesPerPod)
	assert.Equal(t, int64(1), res.TensorParallel)
	assert.Equal(t, 0.5, res.GPUMemoryUtilization)
	assert.Equal(t, int64(4096), res.MaxModelLen)
	assert.Equal(t, backend.CacheSourceUnknown, res.CacheSource)
	assert.Empty(t, res.Node)
	assert.Zero(t, res.BudgetBytes)
	assert.Equal(t, "the fit of preset big is unverified until a backend is registered: it declares 1 GPU, 100.0 GiB of weights and 1.0 GiB of overhead (101.0 GiB required), compute capability 8.9 or newer, --max-model-len 4096", res.Reason)

	// A model id resolves to the single preset serving it, as a backend
	// resolves it; the preset name as the model does too.
	res, err = c.FitCheck(context.Background(), backend.FitRequest{Model: tinyRepo})
	require.NoError(t, err)
	assert.Equal(t, "tiny", res.Preset)
	res, err = c.FitCheck(context.Background(), backend.FitRequest{Model: "tiny"})
	require.NoError(t, err)
	assert.Equal(t, "tiny", res.Preset)

	// The catalog lists the presets as a backend does, the declared compute
	// capability with them.
	presets, err := c.ListPresets(context.Background())
	require.NoError(t, err)
	require.Len(t, presets, 2)
	assert.Equal(t, "big", presets[0].Name)
	assert.Equal(t, "8.9", presets[0].MinComputeCapability)
	assert.Empty(t, presets[1].MinComputeCapability)
}

// What the catalog cannot answer is named: an unknown preset lists the
// published ones (not_found), a model no preset serves and an empty catalog
// stay no_backend.
func TestCatalogFitCheckRefusals(t *testing.T) {
	c := newCatalog(t, presetConfigMap("tiny", presetDoc("tiny", tinyRepo, 0.001, "")))

	_, err := c.FitCheck(context.Background(), backend.FitRequest{Preset: "nope"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, backend.ErrNotFound), err)
	assert.Contains(t, err.Error(), `preset "nope" not found (available: tiny)`)

	_, err = c.FitCheck(context.Background(), backend.FitRequest{Model: "org/unknown"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, backend.ErrNoBackend), err)
	assert.Contains(t, err.Error(), "no serving preset serves org/unknown")
	assert.Contains(t, err.Error(), "(presets: tiny)")

	_, err = c.FitCheck(context.Background(), backend.FitRequest{Preset: "tiny", Model: bigRepo})
	require.Error(t, err)
	assert.True(t, errors.Is(err, backend.ErrInvalid), err)

	empty := newCatalog(t)
	_, err = empty.FitCheck(context.Background(), backend.FitRequest{Preset: "tiny"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, backend.ErrNoBackend), err)
	assert.Contains(t, err.Error(), "no serving preset is published in namespace "+testPlatformNS+" (ConfigMaps labelled "+DefaultPresetSelector+")")
}

// The preset namespace follows the discovery document when one is
// published, the chart values otherwise.
func TestCatalogReadsThePresetNamespaceFromDiscovery(t *testing.T) {
	elsewhere := presetConfigMap("tiny", presetDoc("tiny", tinyRepo, 0.001, ""))
	elsewhere.Namespace = "presets"
	c := newCatalog(t, discoveryConfigMapWith(discoveryOpts{presetNamespace: "presets"}), elsewhere)
	res, err := c.FitCheck(context.Background(), backend.FitRequest{Preset: "tiny"})
	require.NoError(t, err)
	assert.Equal(t, "tiny", res.Preset)

	c, err = NewCatalog(backend.KServeOptions{Clientset: kubefake.NewSimpleClientset(elsewhere), DiscoveryNamespace: testPlatformNS, PresetNamespace: "presets"})
	require.NoError(t, err)
	res, err = c.FitCheck(context.Background(), backend.FitRequest{Preset: "tiny"})
	require.NoError(t, err)
	assert.Equal(t, "tiny", res.Preset)
}
