package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
)

func TestGPUPoolFromArguments(t *testing.T) {
	pool, err := gpuPoolFrom("", "")
	require.NoError(t, err)
	assert.Nil(t, pool)

	pool, err = gpuPoolFrom("nvidia.com/gpu:NoSchedule", "giantswarm.io/machine-pool=c1-gpu01, tier=a")
	require.NoError(t, err)
	assert.Equal(t, &backend.GPUPool{
		Taint:        &backend.Taint{Key: "nvidia.com/gpu", Effect: "NoSchedule"},
		NodeSelector: map[string]string{"giantswarm.io/machine-pool": "c1-gpu01", "tier": "a"},
	}, pool)

	pool, err = gpuPoolFrom("dedicated=llm", "")
	require.NoError(t, err)
	assert.Equal(t, &backend.Taint{Key: "dedicated", Value: "llm"}, pool.Taint, "a value without an effect")
	assert.Nil(t, pool.NodeSelector)

	_, err = gpuPoolFrom(":NoSchedule", "")
	assert.ErrorContains(t, err, "gpuPoolTaint")
	_, err = gpuPoolFrom("", "no-equals")
	assert.ErrorContains(t, err, "gpuPoolNodeSelector")
}
