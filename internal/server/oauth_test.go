package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/providers"
	oauthserver "github.com/giantswarm/mcp-oauth/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/idtoken"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// fakeIdP is a minimal OIDC issuer on https://localhost: a discovery
// document, enough for mcp-oauth's Dex provider to boot.
type fakeIdP struct {
	issuer string
	caFile string
	srv    *httptest.Server
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{}
	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.issuer,
			"authorization_endpoint":                idp.issuer + "/auth",
			"token_endpoint":                        idp.issuer + "/token",
			"userinfo_endpoint":                     idp.issuer + "/userinfo",
			"jwks_uri":                              idp.issuer + "/keys",
			"response_types_supported":              []string{"code"},
			"scopes_supported":                      []string{"openid", "email", "profile", "groups", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		})
	})

	// mcp-oauth rejects IP-literal issuers; a hostname that resolves to loopback
	// (localhost, like the agentlab Dex) boots the provider. httptest's
	// certificate has no localhost SAN, so mint one.
	srv := httptest.NewUnstartedServer(mux)
	cert := selfSignedLocalhost(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	idp.srv = srv
	_, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "https://"))
	require.NoError(t, err)
	idp.issuer = "https://localhost:" + port + "/dex"

	caPath := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))
	idp.caFile = caPath
	return idp
}

func selfSignedLocalhost(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "localhost"},
		DNSNames:              []string{"localhost"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func (f *fakeIdP) config(t *testing.T, downstream bool) OAuthConfig {
	t.Helper()
	return OAuthConfig{Settings: oauthSettings(t, dexEnvironment(f.issuer, f.caFile)), DownstreamOAuth: downstream}
}

// dexEnvironment is the OAUTH_* configuration of a Dex at issuer whose
// forwarded id_tokens for the platform client are accepted.
func dexEnvironment(issuer, caFile string) map[string]string {
	return map[string]string{
		"OAUTH_ISSUER":                              "http://localhost:8080",
		"OAUTH_PROVIDER":                            "dex",
		"OAUTH_DEX_ISSUER_URL":                      issuer,
		"OAUTH_DEX_CLIENT_ID":                       "agent-platform",
		"OAUTH_DEX_CLIENT_SECRET":                   "lab-only",
		"OAUTH_DEX_CA_FILE":                         caFile,
		"OAUTH_TRUSTED_AUDIENCES":                   "agent-platform",
		"OAUTH_ENABLE_CLIENT_ID_METADATA_DOCUMENTS": "true",
	}
}

// oauthSettings reads mcp-oauth's settings from environment the way the
// serve command does.
func oauthSettings(t *testing.T, environment map[string]string) oauthconfig.Settings {
	t.Helper()
	for name, value := range environment {
		t.Setenv(name, value)
	}
	settings, err := oauthconfig.SettingsFromEnv()
	require.NoError(t, err)
	return settings
}

// TestForwardedIDTokenBecomesTheUser is the platform path: muster (or the
// portal through the gateway) forwards the ID token for the platform client;
// once mcp-oauth has validated it, the request carries the user and, with
// downstream OAuth, their ID token.
func TestForwardedIDTokenBecomesTheUser(t *testing.T) {
	idp := newPrivateIdP(t, "")
	configuration := idp.config(t)
	configuration.DownstreamOAuth = true
	o, err := newOAuth(configuration, "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	require.True(t, o.server.Config.EnableClientIDMetadataDocuments)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var seen struct {
		id    *providers.UserInfo
		token string
		ok    bool
	}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = handler.UserInfoFromContext(r.Context())
		seen.token, _ = idtoken.FromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})

	forwarded := idp.idToken(t, []string{"agent-platform"})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+forwarded)
	rec := httptest.NewRecorder()
	o.protect(next).ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the user reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.Equal(t, "sub-admin", seen.id.ID)
	assert.Equal(t, "agent-platform", seen.id.ClientID)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.True(t, seen.id.IsSSO(), "a forwarded id_token")
	assert.Equal(t, forwarded, seen.token, "downstream OAuth: the forwarded id_token is what the Kubernetes API will see")

	// A bearer this server issued carries the IdP id_token stored next to it;
	// without one, downstream OAuth has nothing to act with.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
	req.Header.Set("Authorization", "Bearer issued-by-this-server")
	rec = httptest.NewRecorder()
	o.protect(next).ServeHTTP(rec, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)

	// No validated user, no ID token to attach.
	rec = httptest.NewRecorder()
	attachIDToken(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/models", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestDownstreamOffKeepsTheServiceAccount(t *testing.T) {
	idp := newPrivateIdP(t, "")
	o, err := newOAuth(idp.config(t), "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var attributed string
	var hasToken bool
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if user, ok := handler.UserInfoFromContext(r.Context()); ok {
			attributed = user.Email
		}
		_, hasToken = idtoken.FromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+idp.idToken(t, []string{"agent-platform"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, "admin@lab.local", attributed, "the user is known and attributed")
	assert.False(t, hasToken, "but nothing is presented to the Kubernetes API")
}

func TestProtectedResources(t *testing.T) {
	advertised := []string{"https://dex.example.com"}
	configuration := &oauthserver.Config{Issuer: "https://mm.example.com", ResourceMetadataByPath: map[string]oauthserver.ProtectedResourceConfig{
		"/":    {ResourceIdentifier: "https://mm.example.com", AuthorizationServers: advertised},
		"/mcp": {ResourceIdentifier: "https://mm.example.com/mcp", ScopesSupported: []string{"models"}},
	}}
	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{
		"/":        {ResourceIdentifier: "https://mm.example.com", AuthorizationServers: advertised},
		"/mcp":     {ResourceIdentifier: "https://mm.example.com/mcp", ScopesSupported: []string{"models"}},
		api.Prefix: {AuthorizationServers: advertised},
	}, protectedResources(configuration, "/mcp"), "a configured path keeps its entry; an added one advertises the main resource's servers")
	assert.Len(t, configuration.ResourceMetadataByPath, 2, "the configuration is not changed")

	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{api.Prefix: {}, "/mcp": {}},
		protectedResources(&oauthserver.Config{Issuer: "https://mm.example.com"}, "/mcp"), "nothing advertised: the server's own issuer")

	trailing := &oauthserver.Config{Issuer: "https://mm.example.com", ResourceMetadataByPath: map[string]oauthserver.ProtectedResourceConfig{
		"/mcp/": {ResourceIdentifier: "https://mm.example.com/mcp/"},
	}}
	assert.Equal(t, map[string]oauthserver.ProtectedResourceConfig{"/mcp/": {ResourceIdentifier: "https://mm.example.com/mcp/"}, api.Prefix: {}},
		protectedResources(trailing, "/mcp"), "a configured path with a trailing slash describes the MCP endpoint")
}

// TestServerGuardsRESTAndMCPButNotProbes wires OAuth through the assembled
// server: probes stay open, the API and the MCP endpoint demand a token.
func TestServerGuardsRESTAndMCPButNotProbes(t *testing.T) {
	idp := newFakeIdP(t)
	svc := service.New([]backend.Backend{downBackend{}}, jobs.NewManager(jobs.WithAnonymousAccess(true)), nil, nil, service.Config{}, nil)
	cfg := idp.config(t, false)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true, OAuth: &cfg}, svc, api.NewMCPServer(svc, buildinfo.Info{Version: "test"}), slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { srv.oauth.shutdown(context.Background()) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	status := func(method, path, bearer string) int {
		req, err := http.NewRequest(method, ts.URL+path, nil)
		require.NoError(t, err)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if method == http.MethodPost {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/healthz", ""))
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/readyz", ""))
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodGet, "/api/v1/backend", ""), "REST needs a token")
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodGet, "/api/v1/openapi.yaml", ""))
	assert.Equal(t, http.StatusUnauthorized, status(http.MethodPost, "/mcp", ""), "MCP needs a token")
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/.well-known/oauth-authorization-server", ""), "metadata stays public")
}

// newLocalhostTLSServer serves TLS with a certificate that covers the
// hostname localhost.
func newLocalhostTLSServer(t *testing.T, handler http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{selfSignedLocalhost(t)}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// localhostURL is the server's base URL with the hostname localhost.
func localhostURL(srv *httptest.Server) string {
	return strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)
}
