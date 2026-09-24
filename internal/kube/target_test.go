package kube

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/giantswarm/model-manager/internal/identity"
)

// fakeTargetAPIServer answers like an apiserver that trusts no caller token:
// 401 for any bearer token except "rbac-denied" (403 for a named user), and
// 403 as system:anonymous without one.
func fakeTargetAPIServer(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch token {
		case "":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"configmaps \"x\" is forbidden: User \"system:anonymous\" cannot get resource \"configmaps\" in API group \"\" in the namespace \"ns\"","reason":"Forbidden","code":403}`))
		case "rbac-denied":
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"configmaps \"x\" is forbidden: User \"jane@example.com\" cannot get resource \"configmaps\"","reason":"Forbidden","code":403}`))
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","metadata":{},"status":"Failure","message":"Unauthorized","reason":"Unauthorized","code":401}`))
		}
	}))
	t.Cleanup(srv.Close)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	return srv, ca
}

func testJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

// TestTargetRefusalNamesThePrecondition: every refusal of the target
// apiserver keeps its code and reason and says what to fix.
func TestTargetRefusalNamesThePrecondition(t *testing.T) {
	srv, ca := fakeTargetAPIServer(t)
	tc, err := NewForTarget(srv.URL, ca, nil)
	require.NoError(t, err)
	get := func(ctx context.Context) error {
		_, err := tc.For(ctx).Clientset.CoreV1().ConfigMaps("ns").Get(ctx, "x", metav1.GetOptions{})
		return err
	}

	t.Run("an untrusted token names the issuer and audience", func(t *testing.T) {
		tok := testJWT(t, map[string]any{"iss": "https://dex.installation.example", "aud": "muster", "exp": time.Now().Add(time.Hour).Unix()})
		err := get(identity.ContextWithToken(context.Background(), tok))
		require.Error(t, err)
		assert.True(t, apierrors.IsUnauthorized(err), "the code and reason stay: %v", err)
		assert.Contains(t, err.Error(), "must trust the installation's Dex as an OIDC issuer")
		assert.Contains(t, err.Error(), "structuredAuthentication jwt issuer https://dex.installation.example, audience muster")
		assert.Contains(t, err.Error(), srv.URL)
	})
	t.Run("an expired token says so instead", func(t *testing.T) {
		tok := testJWT(t, map[string]any{"iss": "https://dex.installation.example", "aud": "muster", "exp": time.Now().Add(-time.Minute).Unix()})
		err := get(identity.ContextWithToken(context.Background(), tok))
		assert.True(t, apierrors.IsUnauthorized(err))
		assert.Contains(t, err.Error(), "the caller's token expired at")
		assert.NotContains(t, err.Error(), "must trust")
	})
	t.Run("a call without a caller names the missing token", func(t *testing.T) {
		err := get(context.Background())
		assert.True(t, apierrors.IsForbidden(err), "%v", err)
		assert.Contains(t, err.Error(), "without a caller's token")
		assert.Contains(t, err.Error(), "--downstream-oauth")
	})
	t.Run("the caller's RBAC on the target", func(t *testing.T) {
		err := get(identity.ContextWithToken(context.Background(), "rbac-denied"))
		assert.True(t, apierrors.IsForbidden(err), "%v", err)
		assert.Contains(t, err.Error(), `User "jane@example.com" cannot get resource`)
		assert.Contains(t, err.Error(), "whose RBAC there does not allow this")
	})
}

func TestAuthFailureWithoutClaims(t *testing.T) {
	msg := authFailure("https://api.wc.example:6443", http.StatusUnauthorized, "", "opaque", time.Now())
	assert.Equal(t, "Unauthorized — https://api.wc.example:6443 does not accept the caller's token: the target cluster's apiserver must trust the installation's Dex as an OIDC issuer (structuredAuthentication jwt issuer the installation's Dex, audience the client ID the caller's token is issued for)", msg)
}
