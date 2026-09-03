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

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/identity"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// fakeIdP is a minimal OIDC issuer on https://localhost: discovery document
// and JWKS, enough for mcp-oauth's Dex provider to boot and for forwarded
// id_tokens to be validated against it — the shape of the platform's Dex.
type fakeIdP struct {
	issuer string
	key    *rsa.PrivateKey
	caFile string
	srv    *httptest.Server
}

const testKID = "test-key"

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	idp := &fakeIdP{key: key}
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
	mux.HandleFunc("/dex/keys", func(w http.ResponseWriter, _ *http.Request) {
		jwks := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: testKID, Algorithm: string(jose.RS256), Use: "sig"}}}
		_ = json.NewEncoder(w).Encode(jwks)
	})

	// mcp-oauth rejects IP-literal issuers even with private IPs allowed; a
	// hostname that resolves to loopback (localhost, like the agentlab Dex) is
	// what AllowPrivateIP is for. httptest's certificate has no localhost SAN,
	// so mint one.
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

// idToken mints an id_token the fake IdP signed.
func (f *fakeIdP) idToken(t *testing.T, aud []string, exp time.Time) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.key},
		(&jose.SignerOptions{}).WithHeader("kid", testKID).WithType("JWT"))
	require.NoError(t, err)
	claims := jwt.Claims{Issuer: f.issuer, Subject: "sub-admin", Audience: jwt.Audience(aud),
		Expiry: jwt.NewNumericDate(exp), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}
	extra := map[string]any{"email": "admin@lab.local", "email_verified": true, "name": "Lab Admin", "groups": []string{"platform-admins"}}
	tok, err := jwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	require.NoError(t, err)
	return tok
}

func (f *fakeIdP) config(downstream bool) OAuthConfig {
	return OAuthConfig{
		BaseURL:            "http://localhost:8080",
		Provider:           ProviderDex,
		DexIssuerURL:       f.issuer,
		DexClientID:        "agent-platform",
		DexClientSecret:    "lab-only",
		DexCAFile:          f.caFile,
		DexAllowPrivateIP:  true,
		TrustedAudiences:   []string{"agent-platform"},
		SSOAllowPrivateIPs: true,
		DownstreamOAuth:    downstream,
	}
}

func TestOAuthConfigValidation(t *testing.T) {
	dexOK := OAuthConfig{BaseURL: "https://mm.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}
	require.Error(t, OAuthConfig{}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "http://mm.example.com", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate(), "plain http only on loopback")
	require.NoError(t, OAuthConfig{BaseURL: "http://localhost:8080", DexIssuerURL: "x", DexClientID: "y", DexClientSecret: "z"}.Validate())
	require.NoError(t, dexOK.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: ProviderDex}.Validate(), "dex needs issuer + client")
	require.Error(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: ProviderGoogle, GoogleClientID: "id"}.Validate(), "google needs the secret")
	require.NoError(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: ProviderGoogle, GoogleClientID: "id", GoogleClientSecret: "s", TrustedAudiences: []string{"id"}}.Validate())
	require.Error(t, OAuthConfig{BaseURL: "https://mm.example.com", Provider: "okta"}.Validate(), "unknown provider")
	bad := dexOK
	bad.TrustedAudiences = []string{"agent-platform", " "}
	require.Error(t, bad.Validate(), "empty audience")
}

// TestForwardedIDTokenBecomesTheCaller is the platform path: muster (or the
// portal through the gateway) forwards the IdP id_token for the platform
// client; model-manager validates it against the IdP's JWKS and the request
// carries the caller — and, with downstream OAuth, the caller's token.
func TestForwardedIDTokenBecomesTheCaller(t *testing.T) {
	idp := newFakeIdP(t)
	o, err := newOAuth(idp.config(true), "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var seen struct {
		id    *identity.Identity
		token string
		ok    bool
	}
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.id, seen.ok = identity.FromContext(r.Context())
		seen.token, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))

	call := func(bearer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/models", nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	rec := call("")
	assert.Equal(t, http.StatusUnauthorized, rec.Code, "no token, no API")
	assert.Contains(t, rec.Header().Get("WWW-Authenticate"), "Bearer", "RFC 9728 challenge")

	forwarded := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(30*time.Minute))
	rec = call(forwarded)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	require.True(t, seen.ok, "the caller reaches the handler")
	assert.Equal(t, "admin@lab.local", seen.id.Email)
	assert.Equal(t, "sub-admin", seen.id.Subject)
	assert.Equal(t, []string{"platform-admins"}, seen.id.Groups)
	assert.Equal(t, identity.SourceSSO, seen.id.Source)
	assert.Equal(t, forwarded, seen.token, "downstream OAuth: the forwarded id_token is what the Kubernetes API will see")

	// A token minted for some other client (another aggregator, another app)
	// is not trusted, even though the same IdP signed it.
	assert.Equal(t, http.StatusUnauthorized, call(idp.idToken(t, []string{"someone-else"}, time.Now().Add(30*time.Minute))).Code)
	// Expired tokens are rejected.
	assert.Equal(t, http.StatusUnauthorized, call(idp.idToken(t, []string{"agent-platform"}, time.Now().Add(-time.Minute))).Code)
	// Garbage is rejected.
	assert.Equal(t, http.StatusUnauthorized, call("not-a-token").Code)
}

func TestDownstreamOffKeepsTheServiceAccount(t *testing.T) {
	idp := newFakeIdP(t)
	o, err := newOAuth(idp.config(false), "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() { o.shutdown(context.Background()) })

	var caller string
	var hasToken bool
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = identity.Caller(r.Context())
		_, hasToken = identity.TokenFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+idp.idToken(t, []string{"agent-platform"}, time.Now().Add(time.Minute)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "admin@lab.local", caller, "the caller is known and attributed")
	assert.False(t, hasToken, "but nothing is presented to the Kubernetes API")
}

// TestServerGuardsRESTAndMCPButNotProbes wires OAuth through the assembled
// server: probes stay open, the API and the MCP endpoint demand a token.
func TestServerGuardsRESTAndMCPButNotProbes(t *testing.T) {
	idp := newFakeIdP(t)
	svc := service.New(downBackend{}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	cfg := idp.config(false)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true, OAuth: &cfg}, svc, api.NewMCPServer(svc, "test"), slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	token := idp.idToken(t, []string{"agent-platform"}, time.Now().Add(time.Minute))
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/api/v1/backend", token))
	assert.Equal(t, http.StatusOK, status(http.MethodGet, "/.well-known/oauth-authorization-server", ""), "metadata stays public")
}
