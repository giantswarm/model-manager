package kube

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	ctx := t.Context()
	base, err := c.For(ctx)
	require.NoError(t, err)
	assert.Same(t, c, base, "no token: the ServiceAccount clients")

	valid := jwt(t, time.Now().Add(time.Hour))
	ctx = identity.ContextWithToken(identity.ContextWith(ctx, &identity.Identity{Email: "admin@lab.local"}), valid)
	user, err := c.For(ctx)
	require.NoError(t, err)
	require.NotSame(t, c, user)
	cached, err := c.For(ctx)
	require.NoError(t, err)
	assert.Same(t, user, cached, "cached per token")

	expired := jwt(t, time.Now().Add(-time.Minute))
	ctx = identity.ContextWithToken(t.Context(), expired)
	stale, err := c.For(ctx)
	require.NoError(t, err)
	assert.NotSame(t, c, stale, "an expired caller token is still the caller's: no fallback to the ServiceAccount, the apiserver rejects it")
	cached, err = c.For(ctx)
	require.NoError(t, err)
	assert.Same(t, stale, cached, "and stays cached")

	opaque := identity.ContextWithToken(t.Context(), "opaque")
	user, err = c.For(opaque)
	require.NoError(t, err)
	assert.NotSame(t, c, user, "a token without exp is presented as long as the request lasts")
}

func TestForEvictsExpiredEntriesWhenFull(t *testing.T) {
	c := testClients(t)
	for i := 0; i < maxCallerClients; i++ {
		ctx := identity.ContextWithToken(t.Context(), jwt(t, time.Now().Add(-time.Hour), i))
		_, err := c.For(ctx)
		require.NoError(t, err)
	}
	assert.Len(t, c.byUser, maxCallerClients)
	_, err := c.For(identity.ContextWithToken(t.Context(), jwt(t, time.Now().Add(time.Hour), -1)))
	require.NoError(t, err)
	assert.Len(t, c.byUser, 1, "expired entries are evicted before a new one is cached")
}

func TestForConstructionFailureSendsNoServiceAccountRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"ConfigMapList","apiVersion":"v1","items":[]}`))
	}))
	t.Cleanup(server.Close)
	clients, err := fromRESTConfig(&rest.Config{Host: server.URL, BearerToken: "service-account"})
	require.NoError(t, err)
	clients.restCfg.CAFile = filepath.Join(t.TempDir(), "missing-ca")
	caller, err := clients.For(identity.ContextWithToken(t.Context(), "caller-token"))
	if err == nil {
		_, err = caller.Clientset.CoreV1().ConfigMaps("default").List(t.Context(), metav1.ListOptions{})
	}
	require.ErrorContains(t, err, "caller-token Kubernetes clients")
	require.Nil(t, caller)
	require.Zero(t, requests.Load())
	require.Empty(t, clients.byUser)
}

func TestForExpiredTokenReachesAPIAsCaller(t *testing.T) {
	token := jwt(t, time.Now().Add(-time.Minute))
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		authorization = request.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"Unauthorized","code":401,"message":"expired caller token"}`))
	}))
	t.Cleanup(server.Close)
	clients, err := fromRESTConfig(&rest.Config{Host: server.URL, BearerToken: "service-account"})
	require.NoError(t, err)
	caller, err := clients.For(identity.ContextWithToken(t.Context(), token))
	require.NoError(t, err)
	_, err = caller.Clientset.CoreV1().ConfigMaps("default").List(t.Context(), metav1.ListOptions{})
	require.True(t, apierrors.IsUnauthorized(err), "%v", err)
	require.Equal(t, "Bearer "+token, authorization)
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
