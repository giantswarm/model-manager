package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/identity"
)

// fakeGitHub answers GET /user for the one token it knows and counts calls.
func fakeGitHub(t *testing.T, token, login string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/user" || r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"login":"` + login + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestGitHubPinCarriesBothTokens is the App-pinned registration: the bearer
// is the person's App user token, verified with GET /user once per cache
// lifetime and kept for commit mode; the forwarded ID token in
// X-Muster-Id-Token is validated as a forwarded bearer is without the pin and
// becomes the caller and the Kubernetes token, so apply mode is unchanged.
func TestGitHubPinCarriesBothTokens(t *testing.T) {
	idp := newFakeIdP(t)
	gh, calls := fakeGitHub(t, "ghu_person", "jane")
	cfg := idp.config(true)
	cfg.GitHub = &GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-model-manager", APIURL: gh.URL}
	o, err := newOAuth(cfg, "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var seen struct {
		id    *identity.Identity
		token string
		gh    *identity.GitHub
	}
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, _ = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		seen.gh, _ = identity.GitHubFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}), true)
	call := func(bearer, idToken string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if idToken != "" {
			req.Header.Set(ForwardedIdentityHeader, idToken)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))

	rec := call("ghu_person", forwarded)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.Equal(t, "admin@lab.local", seen.id.Email, "the caller is the person the ID token names")
	assert.Equal(t, forwarded, seen.token, "the Kubernetes API sees the forwarded ID token")
	assert.Equal(t, &identity.GitHub{Login: "jane", Token: "ghu_person"}, seen.gh, "commit mode gets the App user token")

	require.Equal(t, http.StatusNoContent, call("ghu_person", forwarded).Code)
	assert.Equal(t, 1, *calls, "a verified bearer is cached")

	rec = call("ghu_person", "")
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "auth.forwardIdentity: true")

	rec = call("ghu_revoked", forwarded)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "core_auth_login server=model-manager")

	rec = call("", forwarded)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	assert.Equal(t, http.StatusUnauthorized, call("ghu_person", idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute))).Code, "the ID token is validated as without the pin")

	// The REST API is not pinned: the portal calls it with the ID token as
	// the bearer, and no GitHub token is asked for.
	rest := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }), false)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+forwarded)
	restRec := httptest.NewRecorder()
	rest.ServeHTTP(restRec, req)
	assert.Equal(t, http.StatusNoContent, restRec.Code, restRec.Body.String())
}

func TestGitHubPinValidation(t *testing.T) {
	assert.Error(t, GitHubPin{}.Validate())
	assert.Error(t, GitHubPin{AuthorizationServer: "github.com/apps/x"}.Validate())
	assert.NoError(t, GitHubPin{AuthorizationServer: "https://github.com/apps/giantswarm-model-manager"}.Validate())
}
