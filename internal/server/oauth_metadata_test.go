package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResourceSpecificOAuthChallenges(t *testing.T) {
	runtime, err := newOAuth(OAuthConfig{Settings: oauthSettings(t, map[string]string{
		"OAUTH_ISSUER": "https://manager.example.test", "OAUTH_PROVIDER": "google", "OAUTH_GOOGLE_CLIENT_ID": "client",
		"OAUTH_GOOGLE_CLIENT_SECRET": "test-secret", "OAUTH_GOOGLE_REDIRECT_URL": "https://manager.example.test/oauth/callback",
	})}, "/mcp", slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	t.Cleanup(func() { runtime.shutdown(context.WithoutCancel(t.Context())) })
	mux := http.NewServeMux()
	runtime.register(mux)
	for _, resource := range []string{"/mcp", "/api/v1"} {
		mux.Handle(resource, runtime.protect(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("missing credential accepted") })))
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, resource, nil))
		metadataURL := "https://manager.example.test/.well-known/oauth-protected-resource" + resource
		require.Equal(t, http.StatusUnauthorized, response.Code)
		require.Contains(t, response.Header().Get("WWW-Authenticate"), `resource_metadata="`+metadataURL+`"`)
		require.NotContains(t, response.Header().Get("WWW-Authenticate"), "error=")
		metadata := httptest.NewRecorder()
		mux.ServeHTTP(metadata, httptest.NewRequest(http.MethodGet, metadataURL, nil))
		require.Equal(t, http.StatusOK, metadata.Code)
		var body map[string]any
		require.NoError(t, json.Unmarshal(metadata.Body.Bytes(), &body))
		require.Equal(t, "https://manager.example.test"+resource, body["resource"])
	}
}
