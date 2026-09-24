package backend

import (
	"strings"
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

// The pool's instance shapes: the field cluster-manager writes so the fit
// check can judge a model against a pool that has no node yet.
func TestKServeDocumentGPUPoolInstances(t *testing.T) {
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
      instances:
        - {instanceType: g6.xlarge, size: xlarge, vcpu: 4, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: 11.9}
        - {instanceType: g6.2xlarge, vcpu: 8, memoryGiB: 32, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 7, usableMemoryGiB: 27.1}
`
	d, err := ParseDocument([]byte(raw))
	require.NoError(t, err)
	want := []InstanceShape{
		{InstanceType: "g6.xlarge", Size: "xlarge", VCPU: 4, MemoryGiB: 16, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 3, UsableMemoryGiB: 11.9},
		{InstanceType: "g6.2xlarge", VCPU: 8, MemoryGiB: 32, GPUs: 1, GPUMemoryGiB: 24, UsableVCPU: 7, UsableMemoryGiB: 27.1},
	}
	assert.Equal(t, want, d.Spec.KServe.GPUPool.Instances)
	assert.Equal(t, want, d.Options(Options{}).KServe.GPUPool.Instances, "the shapes reach the option")
	assert.Equal(t, "xlarge", want[0].SizeName())
	assert.Equal(t, "2xlarge", want[1].SizeName(), "size defaults to the part after the family")
	assert.Equal(t, "custom", InstanceShape{InstanceType: "custom"}.SizeName())

	rendered, err := d.Render()
	require.NoError(t, err)
	again, err := ParseDocument(rendered)
	require.NoError(t, err)
	assert.Equal(t, d, again, "the shapes survive a render round trip")

	for _, tc := range []struct{ shape, field string }{
		{`{size: xlarge, vcpu: 4, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: 11.9}`, "instances[0].instanceType: required"},
		{`{instanceType: g6.xlarge, vcpu: 0, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: 11.9}`, "instances[0].vcpu: must be positive"},
		{`{instanceType: g6.xlarge, vcpu: 4, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: -1}`, "instances[0].usableMemoryGiB: must be positive"},
		{`{instanceType: g6.xlarge, vcpu: 4, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: 11.9, family: g6}`, "unknown field"},
	} {
		_, err := ParseDocument([]byte(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  kserve:
    target: {servingNamespace: model-serving}
    gpuPool:
      instances:
        - ` + tc.shape + "\n"))
		require.Error(t, err, tc.shape)
		assert.Contains(t, err.Error(), tc.field)
	}
	_, err = ParseDocument([]byte(`apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  kserve:
    target: {servingNamespace: model-serving}
    gpuPool:
      instances:
        - {instanceType: g6.xlarge, vcpu: 4, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: 11.9}
        - {instanceType: g6.2xlarge, vcpu: 8, memoryGiB: 32, gpus: 0, gpuMemoryGiB: 24, usableVcpu: 7, usableMemoryGiB: 27.1}
`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.kserve.gpuPool.instances[1].gpus: must be positive", "the failing entry is named")
}

// The cluster's pools by name (giantswarm/cluster-manager#89,
// giantswarm/model-manager#152): the form cluster-manager writes with two or
// more pools, read strictly and handed to the kserve backend as the option;
// a pool's bad shape is refused naming the pool.
func TestKServeDocumentGPUPools(t *testing.T) {
	raw := `apiVersion: agent-platform.giantswarm.io/v1alpha1
kind: ModelBackend
spec:
  kind: kserve
  source: cluster-manager
  kserve:
    target: {cluster: local, servingNamespace: model-serving}
    gpuPools:
      c1-gpu-l4:
        instances:
          - {instanceType: g6.xlarge, size: xlarge, vcpu: 4, memoryGiB: 16, gpus: 1, gpuMemoryGiB: 24, usableVcpu: 3, usableMemoryGiB: 11.9}
      c1-gpu-l40s:
        instances:
          - {instanceType: g6e.2xlarge, size: 2xlarge, vcpu: 8, memoryGiB: 64, gpus: 1, gpuMemoryGiB: 48, usableVcpu: 7, usableMemoryGiB: 57.5}
`
	d, err := ParseDocument([]byte(raw))
	require.NoError(t, err)
	require.Len(t, d.Spec.KServe.GPUPools, 2)
	assert.Nil(t, d.Spec.KServe.GPUPool, "no pool pins every predictor")
	pools := d.Options(Options{}).KServe.GPUPools
	require.Contains(t, pools, "c1-gpu-l40s")
	assert.Equal(t, "g6e.2xlarge", pools["c1-gpu-l40s"].Instances[0].InstanceType)

	_, err = ParseDocument([]byte(strings.Replace(raw, "gpus: 1, gpuMemoryGiB: 48", "gpus: 0, gpuMemoryGiB: 48", 1)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "spec.kserve.gpuPools.c1-gpu-l40s.instances[0].")
}
