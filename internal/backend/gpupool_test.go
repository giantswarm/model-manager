package backend

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKServeDocumentGPUPool(t *testing.T) {
	raw := `apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  source: cluster-manager
  kserve:
    target: {cluster: local, servingNamespace: model-serving}
    gpuPool:
      taint: {key: nvidia.com/gpu, effect: NoSchedule}
      nodeSelector: {giantswarm.io/machine-pool: c1-gpu01}
`
	d, err := ParseDocument([]byte(raw))
	require.NoError(t, err)
	want := &GPUPool{Taint: &Taint{Key: "nvidia.com/gpu", Effect: "NoSchedule"}, NodeSelector: map[string]string{"giantswarm.io/machine-pool": "c1-gpu01"}}
	assert.Equal(t, want, d.Spec.KServe.GPUPool)
	assert.Equal(t, *want, d.Options(Options{}).KServe.GPUPool, "the document's block becomes the option")

	base := Options{KServe: KServeOptions{GPUPool: GPUPool{Taint: &Taint{Key: "keep"}}}}
	d.Spec.KServe.GPUPool = nil
	assert.Equal(t, base.KServe.GPUPool, d.Options(base).KServe.GPUPool, "no block: the base option stays")

	_, err = ParseDocument([]byte(raw + "      unknown: 1\n"))
	require.Error(t, err, "unknown fields are refused")
	_, err = ParseDocument([]byte(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  kserve:
    target: {servingNamespace: model-serving}
    gpuPool:
      taint: {key: nvidia.com/gpu, effect: Sometimes}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.kserve.gpuPool.taint.effect")
	_, err = ParseDocument([]byte(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  kserve:
    target: {servingNamespace: model-serving}
    gpuPool:
      taint: {effect: NoSchedule}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.kserve.gpuPool.taint.key: required")
}
