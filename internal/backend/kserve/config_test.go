package kserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/backend"
)

// The settings cache predating the discovery document
// (giantswarm/model-manager#127): the serving slice publishes the ConfigMap
// while it installs, and settings resolved a moment before it appeared know
// no GPU pool. Resolved without the document, the settings stand for
// DiscoveryAbsentTTL — not DiscoveryTTL — so the document is read soon after
// it appears; settings with the document stand for the full TTL.
func TestSettingsWithoutTheDiscoveryDocumentStandBriefly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	now := time.Now()
	f.b.cfg.now = func() time.Time { return now }
	require.NoError(t, f.cs.CoreV1().ConfigMaps(testPlatformNS).Delete(ctx, DefaultDiscoveryConfigMap, metav1.DeleteOptions{}))
	f.resetSettings()

	s := f.b.cfg.settings(ctx)
	assert.False(t, s.DiscoveryFound)
	assert.Empty(t, s.DiscoveryError, "an absent document is a result, not an error")
	assert.Empty(t, s.GPUPool.NodeSelector)
	require.NotNil(t, f.b.cfg.cached, "the result is cached — briefly")
	assert.False(t, f.b.cfg.recheckDiscovery(ctx), "a recheck while the document is still absent finds nothing")

	// The slice publishes the document. Within DiscoveryAbsentTTL the cache
	// still answers; after it — long before DiscoveryTTL — the document is read.
	_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, discoveryConfigMapWith(discoveryOpts{gpuPool: poolInput()}), metav1.CreateOptions{})
	require.NoError(t, err)
	now = now.Add(DiscoveryAbsentTTL - time.Second)
	assert.False(t, f.b.cfg.settings(ctx).DiscoveryFound, "within the short TTL the cache answers")
	now = now.Add(2 * time.Second)
	s = f.b.cfg.settings(ctx)
	assert.True(t, s.DiscoveryFound, "after it the document is read")
	assert.Equal(t, poolInput().NodeSelector, s.GPUPool.NodeSelector)

	// Settings with the document stand for the full TTL.
	require.NoError(t, f.cs.CoreV1().ConfigMaps(testPlatformNS).Delete(ctx, DefaultDiscoveryConfigMap, metav1.DeleteOptions{}))
	now = now.Add(DefaultDiscoveryTTL - time.Second)
	assert.True(t, f.b.cfg.settings(ctx).DiscoveryFound, "a document that was read stands for DiscoveryTTL")
	assert.False(t, f.b.cfg.recheckDiscovery(ctx), "nothing to recheck while the cached settings hold the document")
	now = now.Add(2 * time.Second)
	assert.False(t, f.b.cfg.settings(ctx).DiscoveryFound)
}

// The settings cache predating the pool's node selector
// (giantswarm/model-manager#127): cluster-manager registers a new pool's
// instance shapes with the backend document, and the serving slice names the
// pool in its discovery document only when the connectivity child renders it
// again. Settings resolved from a document that names no pool while the
// shapes are known stand for PoolUnnamedTTL, are rechecked before a refusal
// and make the refusal say to retry; once the pool is named they stand for
// the full TTL. Without the shapes, a document naming no pool is the verdict.
func TestSettingsWhoseDocumentNamesNoPoolYetStandBriefly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	now := time.Now()
	f.b.cfg.now = func() time.Time { return now }
	f.b.cfg.opts.GPUPool = backend.GPUPool{Instances: []backend.InstanceShape{shapeXLarge}}
	f.setDiscoveryOpts(ctx, discoveryOpts{})

	s := f.b.cfg.settings(ctx)
	require.True(t, s.DiscoveryFound, "the slice's document is published")
	assert.True(t, s.poolUnnamed())
	assert.Equal(t, "the serving layer's discovery document "+testPlatformNS+"/"+DefaultDiscoveryConfigMap+" names no GPU pool yet (no spec.gpuPool.nodeSelector) though the pool's instance shapes are known — the slice is still publishing the new pool; retry in a moment", f.b.cfg.discoveryMissing(s))
	assert.False(t, f.b.cfg.recheckDiscovery(ctx), "a recheck while the document still names no pool finds nothing")

	// The connectivity child renders the pool. Within PoolUnnamedTTL the
	// cache still answers; after it — long before DiscoveryTTL — the pool is read.
	f.renderDiscovery(ctx, discoveryOpts{gpuPool: poolInput()})
	now = now.Add(PoolUnnamedTTL - time.Second)
	assert.Empty(t, f.b.cfg.settings(ctx).GPUPool.NodeSelector, "within the short TTL the cache answers")
	now = now.Add(2 * time.Second)
	s = f.b.cfg.settings(ctx)
	assert.Equal(t, poolInput().NodeSelector, s.GPUPool.NodeSelector, "after it the pool is read")
	assert.Equal(t, []backend.InstanceShape{shapeXLarge}, s.GPUPool.Instances, "the registered shapes stand beside the document's selector")
	assert.Empty(t, f.b.cfg.discoveryMissing(s))

	// Settings naming the pool stand for the full TTL.
	f.renderDiscovery(ctx, discoveryOpts{})
	now = now.Add(DefaultDiscoveryTTL - time.Second)
	assert.Equal(t, poolInput().NodeSelector, f.b.cfg.settings(ctx).GPUPool.NodeSelector, "a pool that was read stands for DiscoveryTTL")
	assert.False(t, f.b.cfg.recheckDiscovery(ctx), "nothing to recheck while the cached settings name the pool")

	// Without the shapes nothing says a pool is coming: the document is the
	// verdict, for the full TTL, and nothing is missing.
	f.b.cfg.opts.GPUPool = backend.GPUPool{}
	f.resetSettings()
	s = f.b.cfg.settings(ctx)
	require.True(t, s.DiscoveryFound)
	assert.False(t, s.poolUnnamed())
	assert.Empty(t, f.b.cfg.discoveryMissing(s))
	assert.False(t, f.b.cfg.recheckDiscovery(ctx))
	now = now.Add(PoolUnnamedTTL + time.Second)
	assert.True(t, f.b.cfg.fresh(), "a document naming no pool without shapes stands for DiscoveryTTL")
}

// The settings cache predating the llm-d control plane
// (giantswarm/model-manager#148): the serving slice's CRD and runtime-configs
// children land seconds after its discovery document, and settings resolved
// in between say nothing would reconcile a load. Resolved without the control
// plane, the settings stand for ControlPlaneAbsentTTL — not DiscoveryTTL — so
// get_backend and check_fit see it soon after it lands; settings with the
// control plane stand for the full TTL.
func TestSettingsWithoutTheControlPlaneStandBriefly(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	now := time.Now()
	f.b.cfg.now = func() time.Time { return now }
	f.dropControlPlane(ctx)

	s := f.b.cfg.settings(ctx)
	require.True(t, s.DiscoveryFound, "the slice's document is published")
	require.True(t, s.LLMServed, "the CRDs stand")
	assert.Empty(t, s.ControlPlane, "the runtime configs have not landed")
	assert.True(t, s.controlPlaneAbsent())
	require.NotNil(t, f.b.cfg.cached, "the result is cached — briefly")

	// The runtime configs land. Within ControlPlaneAbsentTTL the cache still
	// answers; after it — long before DiscoveryTTL — the control plane is seen.
	_, err := f.dyn.Resource(llmisvcConfigGVR).Namespace(testControlPlaneNS).Create(ctx, wellKnownConfig(), metav1.CreateOptions{})
	require.NoError(t, err)
	now = now.Add(ControlPlaneAbsentTTL - time.Second)
	assert.Empty(t, f.b.cfg.settings(ctx).ControlPlane, "within the short TTL the cache answers")
	now = now.Add(2 * time.Second)
	assert.Equal(t, testControlPlaneNS, f.b.cfg.settings(ctx).ControlPlane, "after it the control plane is seen")

	// Settings with the control plane stand for the full TTL.
	require.NoError(t, f.dyn.Resource(llmisvcConfigGVR).Namespace(testControlPlaneNS).Delete(ctx, wellKnownTemplateConfig, metav1.DeleteOptions{}))
	now = now.Add(DefaultDiscoveryTTL - time.Second)
	assert.Equal(t, testControlPlaneNS, f.b.cfg.settings(ctx).ControlPlane, "a control plane that was seen stands for DiscoveryTTL")
	now = now.Add(2 * time.Second)
	assert.Empty(t, f.b.cfg.settings(ctx).ControlPlane)

	// The API itself not served — the slice's CRD child still landing — is
	// cached as briefly.
	f.dropLLMAPI()
	require.False(t, f.b.cfg.settings(ctx).LLMServed)
	now = now.Add(ControlPlaneAbsentTTL - time.Second)
	assert.True(t, f.b.cfg.fresh())
	now = now.Add(2 * time.Second)
	assert.False(t, f.b.cfg.fresh(), "settings without the API stand for ControlPlaneAbsentTTL")
}

// recheckDiscovery bypasses the cache: a document created a moment after the
// settings were cached without it is read at once, and the fresh settings
// are what the next call gets. Without a configured discovery ConfigMap
// there is nothing to recheck and nothing missing.
func TestRecheckDiscoveryReadsADocumentThatJustAppeared(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	require.NoError(t, f.cs.CoreV1().ConfigMaps(testPlatformNS).Delete(ctx, DefaultDiscoveryConfigMap, metav1.DeleteOptions{}))
	f.resetSettings()
	s := f.b.cfg.settings(ctx)
	require.False(t, s.DiscoveryFound)
	assert.Equal(t, "the serving layer's discovery document "+testPlatformNS+"/"+DefaultDiscoveryConfigMap+" is not published yet — the slice is still installing; retry in a moment", f.b.cfg.discoveryMissing(s))

	_, err := f.cs.CoreV1().ConfigMaps(testPlatformNS).Create(ctx, discoveryConfigMapWith(discoveryOpts{gpuPool: poolInput()}), metav1.CreateOptions{})
	require.NoError(t, err)
	assert.True(t, f.b.cfg.recheckDiscovery(ctx), "the document is there now")
	s = f.b.cfg.settings(ctx)
	assert.True(t, s.DiscoveryFound)
	assert.Equal(t, poolInput().NodeSelector, s.GPUPool.NodeSelector)
	assert.Empty(t, f.b.cfg.discoveryMissing(s))

	// A document that names no pool while the pool's shapes are known is
	// re-read the same way, and the recheck says so once it names the pool.
	f.b.cfg.opts.GPUPool = backend.GPUPool{Instances: []backend.InstanceShape{shapeXLarge}}
	f.setDiscoveryOpts(ctx, discoveryOpts{})
	require.True(t, f.b.cfg.settings(ctx).poolUnnamed())
	f.renderDiscovery(ctx, discoveryOpts{gpuPool: poolInput()})
	assert.True(t, f.b.cfg.recheckDiscovery(ctx), "the document names the pool now")
	assert.Equal(t, poolInput().NodeSelector, f.b.cfg.settings(ctx).GPUPool.NodeSelector)

	f.b.cfg.opts.DiscoveryConfigMap = ""
	f.resetSettings()
	s = f.b.cfg.settings(ctx)
	assert.False(t, s.DiscoveryFound)
	assert.Empty(t, f.b.cfg.discoveryMissing(s), "flags and defaults are the settings; nothing is missing")
	assert.False(t, f.b.cfg.recheckDiscovery(ctx))
}
