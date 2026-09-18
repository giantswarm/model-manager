package kserve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

	f.b.cfg.opts.DiscoveryConfigMap = ""
	f.resetSettings()
	s = f.b.cfg.settings(ctx)
	assert.False(t, s.DiscoveryFound)
	assert.Empty(t, f.b.cfg.discoveryMissing(s), "flags and defaults are the settings; nothing is missing")
	assert.False(t, f.b.cfg.recheckDiscovery(ctx))
}
