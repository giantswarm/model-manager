package server

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/requester"
)

func TestAttachIdentityRequiresCompleteValidatedClaims(t *testing.T) {
	for _, test := range []struct {
		name    string
		user    providers.UserInfo
		allowed bool
	}{
		{"dex", providers.UserInfo{Issuer: "https://dex.example.test", ID: "same-subject", Email: "same@example.test"}, true},
		{"google", providers.UserInfo{Issuer: "https://accounts.google.com", ID: "same-subject", Email: "same@example.test"}, true},
		{"changed email", providers.UserInfo{Issuer: "https://dex.example.test", ID: "same-subject", Email: "changed@example.test"}, true},
		{"delegated", providers.UserInfo{Issuer: "https://dex.example.test", ID: "same-subject", ActorIssuer: "https://actor.example.test", ActorSubject: "agent"}, true},
		{"missing issuer", providers.UserInfo{ID: "subject"}, false},
		{"missing subject", providers.UserInfo{Issuer: "https://dex.example.test"}, false},
		{"missing actor issuer", providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject", ActorSubject: "agent"}, false},
		{"missing actor subject", providers.UserInfo{Issuer: "https://dex.example.test", ID: "subject", ActorIssuer: "https://actor.example.test"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime := &oauthRuntime{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
			called := false
			protected := afterValidation(&test.user, runtime.admitCaller(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				called = true
				caller, ok := requester.FromContext(request.Context())
				require.True(t, ok)
				require.Equal(t, test.user.Issuer, caller.Issuer)
				require.Equal(t, test.user.ID, caller.ID)
				require.Equal(t, test.user.ActorIssuer, caller.ActorIssuer)
				require.Equal(t, test.user.ActorSubject, caller.ActorSubject)
				writer.WriteHeader(http.StatusNoContent)
			})))
			recorder := httptest.NewRecorder()
			protected.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/mcp", nil))
			require.Equal(t, test.allowed, called)
			if test.allowed {
				require.Equal(t, http.StatusNoContent, recorder.Code)
			} else {
				require.Equal(t, http.StatusUnauthorized, recorder.Code)
			}
		})
	}
}
