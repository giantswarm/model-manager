package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/giantswarm/model-manager/internal/identity"
)

func testClients(t *testing.T) *Clients {
	t.Helper()
	c, err := fromRESTConfig(&rest.Config{ // #nosec G101 -- test fixture, not a credential
		Host:            "https://kubernetes.example.test:6443",
		BearerToken:     "service-account-token",
		BearerTokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token",
		UserAgent:       "model-manager",
		TLSClientConfig: rest.TLSClientConfig{Insecure: true},
	})
	require.NoError(t, err)
	return c
}

func TestForTokenDropsEveryServiceAccountCredential(t *testing.T) {
	c := testClients(t)
	user, err := c.ForToken("user-id-token")
	require.NoError(t, err)
	assert.Equal(t, "user-id-token", user.restCfg.BearerToken)
	assert.Empty(t, user.restCfg.BearerTokenFile, "the pod's token file must not shadow the caller's token")
	assert.Equal(t, c.restCfg.Host, user.restCfg.Host)
	assert.Equal(t, c.restCfg.TLSClientConfig, user.restCfg.TLSClientConfig)
	assert.Equal(t, "model-manager", user.restCfg.UserAgent)
	assert.NotSame(t, c.Clientset, user.Clientset)

	_, err = c.ForToken("")
	require.Error(t, err)
}

func TestForUsesCallerWhileTheTokenIsValid(t *testing.T) {
	c := testClients(t)
	ctx := context.Background()
	assert.Same(t, c, c.For(ctx), "no token: the ServiceAccount clients")

	valid := jwt(t, time.Now().Add(time.Hour))
	ctx = identity.ContextWithToken(identity.ContextWith(ctx, &identity.Identity{Email: "admin@lab.local"}), valid)
	user := c.For(ctx)
	require.NotSame(t, c, user)
	assert.Same(t, user, c.For(ctx), "cached per token")

	expired := jwt(t, time.Now().Add(-time.Minute))
	ctx = identity.ContextWithToken(context.Background(), expired)
	stale := c.For(ctx)
	assert.NotSame(t, c, stale, "an expired caller token is still the caller's: no fallback to the ServiceAccount, the apiserver rejects it")
	assert.Same(t, stale, c.For(ctx), "and stays cached")

	opaque := identity.ContextWithToken(context.Background(), "opaque")
	assert.NotSame(t, c, c.For(opaque), "a token without exp is presented as long as the request lasts")
}

func TestForEvictsExpiredEntriesWhenFull(t *testing.T) {
	c := testClients(t)
	for i := 0; i < maxCallerClients; i++ {
		ctx := identity.ContextWithToken(context.Background(), jwt(t, time.Now().Add(-time.Hour), i))
		c.For(ctx)
	}
	assert.Len(t, c.byUser, maxCallerClients)
	c.For(identity.ContextWithToken(context.Background(), jwt(t, time.Now().Add(time.Hour), -1)))
	assert.Len(t, c.byUser, 1, "expired entries are evicted before a new one is cached")
}

func jwt(t *testing.T, exp time.Time, salt ...int) string {
	t.Helper()
	claims := map[string]any{"exp": exp.Unix()}
	if len(salt) > 0 {
		claims["jti"] = salt[0]
	}
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}
