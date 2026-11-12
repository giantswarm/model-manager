package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/giantswarm/mcp-oauth/providers/oidc"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/gitops"
	"github.com/giantswarm/model-manager/internal/identity"
)

// JWTs with a placeholder signature: the server validated the bearer before
// the exchange reads it.
var (
	personToken    = unsignedJWT(`{"sub":"alice"}`)
	delegatedToken = unsignedJWT(`{"sub":"alice","act":{"sub":"agent"}}`)
)

func unsignedJWT(claims string) string {
	encode := base64.RawURLEncoding.EncodeToString
	return encode([]byte(`{"alg":"RS256"}`)) + "." + encode([]byte(claims)) + ".signature"
}

// brokerRequest is what the test broker and GitHub API saw of one request.
type brokerRequest struct {
	method, path, authorization, forwardedIdentity string
	form                                           url.Values
	client, secret                                 string
	basicAuth                                      bool
}

// recorder keeps the requests a test server received, for assertions on the
// test goroutine.
type recorder struct {
	mu       sync.Mutex
	requests []brokerRequest
}

func (r *recorder) record(request *http.Request) {
	seen := brokerRequest{method: request.Method, path: request.URL.Path, authorization: request.Header.Get("Authorization"), forwardedIdentity: request.Header.Get("X-Muster-Id-Token")}
	if request.ParseForm() == nil {
		seen.form = request.PostForm
	}
	seen.client, seen.secret, seen.basicAuth = request.BasicAuth()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, seen)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = nil
}

func (r *recorder) on(path string) []brokerRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	var matching []brokerRequest
	for _, request := range r.requests {
		if request.path == path {
			matching = append(matching, request)
		}
	}
	return matching
}

func (r *recorder) paths() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var paths []string
	for _, request := range r.requests {
		paths = append(paths, request.path)
	}
	return paths
}

func TestGitHubExchange(t *testing.T) {
	type expectation struct {
		grantRequired bool
		sentinel      error
		exchanges     int
		warned        bool
	}
	for scenario, want := range map[string]expectation{
		"success":                {exchanges: 2},
		"delegated actor":        {sentinel: backend.ErrForbidden},
		"missing identity token": {sentinel: gitops.ErrAuthRequired},
		"no grant":               {grantRequired: true, exchanges: 1, warned: true},
		"client refused":         {exchanges: 1, warned: true},
		"broker error":           {sentinel: backend.ErrUnavailable, exchanges: 1, warned: true},
		"redirect":               {exchanges: 1, warned: true},
		"invalid token type":     {exchanges: 1},
		"refresh token":          {exchanges: 1},
		"missing expiry":         {exchanges: 1},
		"GitHub refuses":         {grantRequired: true, exchanges: 1},
		"GitHub fails":           {sentinel: backend.ErrUnavailable, exchanges: 1},
	} {
		t.Run(scenario, func(t *testing.T) {
			secretFile := filepath.Join(t.TempDir(), "client-secret")
			require.NoError(t, os.WriteFile(secretFile, []byte("fixture-secret"), 0o600))
			var seen recorder
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen.record(r)
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/token" {
					switch scenario {
					case "no grant":
						w.WriteHeader(http.StatusBadRequest)
						_, _ = fmt.Fprint(w, `{"error":"invalid_target","error_description":"the requested audience cannot be served"}`)
						return
					case "client refused":
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = fmt.Fprint(w, `{"error":"invalid_client","error_description":"client authentication failed"}`)
						return
					case "broker error":
						w.WriteHeader(http.StatusInternalServerError)
						_, _ = fmt.Fprint(w, "do not expose verified-id-token fixture-secret")
						return
					case "redirect":
						w.Header().Set("Location", "/redirected")
						w.WriteHeader(http.StatusTemporaryRedirect)
						return
					}
					result := oidc.TokenExchangeResponse{AccessToken: "fixture-user-grant", TokenType: "Bearer", IssuedTokenType: oidc.TokenTypeAccessToken, ExpiresIn: 300} // #nosec G101 -- synthetic grants served by the test broker
					switch scenario {
					case "invalid token type":
						result.IssuedTokenType = oidc.TokenTypeIDToken
					case "refresh token":
						result.RefreshToken = "fixture-refresh"
					case "missing expiry":
						result.ExpiresIn = 0
					}
					_ = json.NewEncoder(w).Encode(result) // #nosec G117 -- the fixture is a token endpoint
					return
				}
				switch scenario {
				case "GitHub refuses":
					w.WriteHeader(http.StatusUnauthorized)
					return
				case "GitHub fails":
					w.WriteHeader(http.StatusBadGateway)
					return
				}
				_, _ = fmt.Fprint(w, `{"login":"alice"}`)
			})
			upstream := httptest.NewTLSServer(handler)
			defer upstream.Close()
			client := upstream.Client()
			client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
			configuration := GitHubExchangeConfig{TokenEndpoint: upstream.URL + "/token", ClientID: "manager-client", ClientSecretFile: secretFile, Audience: "manager-app", APIURL: upstream.URL}
			var logs bytes.Buffer
			log := slog.New(slog.NewTextHandler(&logs, nil))
			ctx := identity.ContextWith(t.Context(), &identity.Identity{Subject: "alice"})
			switch scenario {
			case "delegated actor":
				ctx = identity.ContextWithToken(ctx, delegatedToken)
			case "missing identity token":
			default:
				ctx = identity.ContextWithToken(ctx, personToken)
			}
			resolve := newGitHubExchange(configuration, client, log)
			result, err := resolve(ctx)
			if scenario == "success" {
				require.NoError(t, err)
				require.Equal(t, &identity.GitHub{Login: "alice", Token: "fixture-user-grant"}, result) // #nosec G101 -- synthetic broker response
				_, err = resolve(ctx)
				require.NoError(t, err)
				require.Len(t, seen.on("/user"), 2, "user grants are not cached")
			} else {
				require.Error(t, err)
				require.Nil(t, result)
				for _, value := range []string{personToken, "fixture-secret", "fixture-user-grant", "fixture-refresh"} {
					require.NotContains(t, err.Error(), value)
				}
				grant, isGrant := errors.AsType[*gitops.ErrGrantRequired](err)
				require.Equal(t, want.grantRequired, isGrant, "%v", err)
				if isGrant {
					require.Equal(t, "manager-app", grant.App, "grant_required names the audience")
				}
				if want.sentinel != nil {
					require.ErrorIs(t, err, want.sentinel)
				}
			}

			exchanges := seen.on("/token")
			require.Len(t, exchanges, want.exchanges)
			for _, exchange := range exchanges {
				require.Equal(t, http.MethodPost, exchange.method)
				require.Equal(t, oidc.GrantTypeTokenExchange, exchange.form.Get("grant_type"))
				require.Equal(t, personToken, exchange.form.Get("subject_token"))
				require.Equal(t, oidc.TokenTypeIDToken, exchange.form.Get("subject_token_type"))
				require.Equal(t, "manager-app", exchange.form.Get("audience"))
				require.Empty(t, exchange.form.Get("actor_token"))
				require.True(t, exchange.basicAuth)
				require.Equal(t, "manager-client", exchange.client)
				require.Equal(t, "fixture-secret", exchange.secret)
			}
			for _, path := range seen.paths() {
				require.Contains(t, []string{"/token", "/user"}, path, "credentials must never follow a redirect")
			}
			for _, verification := range seen.on("/user") {
				require.Empty(t, verification.forwardedIdentity)
				require.Equal(t, "Bearer fixture-user-grant", verification.authorization)
			}
			require.Equal(t, want.warned, strings.Contains(logs.String(), "GitHub token exchange refused"), logs.String())
			if scenario == "client refused" {
				require.Contains(t, logs.String(), `error=invalid_client errorDescription="client authentication failed"`)
				require.Contains(t, err.Error(), "invalid_client")
			}
			require.NotContains(t, logs.String(), "fixture-secret")
			require.NotContains(t, logs.String(), personToken)
		})
	}
}

func TestGitHubExchangeConfiguration(t *testing.T) {
	valid := GitHubExchangeConfig{TokenEndpoint: "https://broker.example.com/oauth/token", ClientID: "manager", ClientSecretFile: "/mounted/client-secret", Audience: "manager-app"} // #nosec G101 -- configuration fixture with a path, no secret value
	require.NoError(t, valid.Validate())
	for _, endpoint := range []string{"http://broker.example.com/token", "https://user:password@broker.example.com/token", "https://broker.example.com/token?credential=value", "https://broker.example.com/token#fragment", "/token"} {
		configuration := valid
		configuration.TokenEndpoint = endpoint
		require.Error(t, configuration.Validate())
	}
	configuration := valid
	configuration.ClientSecretFile = ""
	require.Error(t, configuration.Validate())
	configuration = valid
	configuration.Audience = ""
	require.Error(t, configuration.Validate())
	configuration = valid
	configuration.ClientID = ""
	require.Error(t, configuration.Validate())
	configuration.ClientIDFile = "/mounted/client-id"
	require.NoError(t, configuration.Validate())
}

func TestGitHubExchangeClientID(t *testing.T) {
	var seen recorder
	upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.record(r)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/token" {
			_ = json.NewEncoder(w).Encode(oidc.TokenExchangeResponse{AccessToken: "fixture-user-grant", TokenType: "Bearer", IssuedTokenType: oidc.TokenTypeAccessToken, ExpiresIn: 300}) // #nosec G101,G117 -- synthetic broker response
			return
		}
		_, _ = fmt.Fprint(w, `{"login":"alice"}`)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}), 0o600))
	secretFile := filepath.Join(dir, "client-secret")
	require.NoError(t, os.WriteFile(secretFile, []byte("fixture-secret"), 0o600))
	idFile := filepath.Join(dir, "client-id")
	require.NoError(t, os.WriteFile(idFile, []byte("file-client\n"), 0o600))
	emptyFile := filepath.Join(dir, "empty")
	require.NoError(t, os.WriteFile(emptyFile, nil, 0o600))
	base := GitHubExchangeConfig{TokenEndpoint: upstream.URL + "/token", ClientSecretFile: secretFile, Audience: "manager-app", CAFile: caFile, APIURL: upstream.URL}
	ctx := identity.ContextWithToken(identity.ContextWith(t.Context(), &identity.Identity{Subject: "alice"}), personToken)

	for _, tc := range []struct {
		name, clientID, clientIDFile, want string
	}{
		{name: "file", clientIDFile: idFile, want: "file-client"},
		{name: "flag wins over file", clientID: "flag-client", clientIDFile: idFile, want: "flag-client"},
		{name: "flag without file", clientID: "flag-client", clientIDFile: filepath.Join(dir, "missing"), want: "flag-client"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen.reset()
			configuration := base
			configuration.ClientID, configuration.ClientIDFile = tc.clientID, tc.clientIDFile
			resolve, err := NewGitHubExchange(configuration, slog.New(slog.DiscardHandler))
			require.NoError(t, err)
			_, err = resolve(ctx)
			require.NoError(t, err)
			exchanges := seen.on("/token")
			require.Len(t, exchanges, 1)
			require.Equal(t, tc.want, exchanges[0].client)
		})
	}

	for name, file := range map[string]string{"missing file": filepath.Join(dir, "missing"), "empty file": emptyFile} {
		t.Run(name, func(t *testing.T) {
			configuration := base
			configuration.ClientIDFile = file
			_, err := NewGitHubExchange(configuration, slog.New(slog.DiscardHandler))
			require.Error(t, err)
		})
	}
}
