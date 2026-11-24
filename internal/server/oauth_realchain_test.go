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
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/identity"
)

// privateIdP is an OIDC issuer the production chain reaches as it reaches
// Dex on a private management cluster: a hostname that resolves to an
// RFC 1918 address of this machine, over TLS with its own CA. The JWKS is on
// the issuer's host, or on a second host when keysHost is set.
type privateIdP struct {
	issuer string
	key    *rsa.PrivateKey
	caFile string
}

const (
	privateIssuerHost = "idp.test"
	privateKeysHost   = "keys.test"
	privateTestKID    = "private-key"
)

// newPrivateIdP serves discovery and JWKS on an RFC 1918 address of this
// machine and answers DNS for idp.test and keys.test with it, through
// net.DefaultResolver, which mcp-oauth's guarded and plain dialers both use.
// It skips when the machine has no RFC 1918 IPv4 address.
func newPrivateIdP(t *testing.T, keysHost string) *privateIdP {
	t.Helper()
	ip := privateIPv4(t)
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	idp := &privateIdP{key: key}

	listener, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	require.NoError(t, err)
	port := listener.Addr().(*net.TCPAddr).Port
	origin := func(host string) string { return "https://" + net.JoinHostPort(host, strconv.Itoa(port)) }
	idp.issuer = origin(privateIssuerHost) + "/dex"
	jwksURI := idp.issuer + "/keys"
	if keysHost != "" {
		jwksURI = origin(keysHost) + "/dex/keys"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/dex/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.issuer,
			"authorization_endpoint":                idp.issuer + "/auth",
			"token_endpoint":                        idp.issuer + "/token",
			"userinfo_endpoint":                     idp.issuer + "/userinfo",
			"jwks_uri":                              jwksURI,
			"response_types_supported":              []string{"code"},
			"scopes_supported":                      []string{"openid", "email", "profile", "groups", "offline_access"},
			"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
			"code_challenge_methods_supported":      []string{"S256"},
			"token_endpoint_auth_methods_supported": []string{"client_secret_basic"},
		})
	})
	mux.HandleFunc("/dex/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: privateTestKID, Algorithm: string(jose.RS256), Use: "sig"},
		}})
	})

	cert := selfSignedFor(t, []string{privateIssuerHost, privateKeysHost})
	srv := httptest.NewUnstartedServer(mux)
	srv.Listener = listener
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	idp.caFile = filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(idp.caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]}), 0o600))

	bypassProxy(t, privateIssuerHost, privateKeysHost)
	resolveTestHosts(t, ip, privateIssuerHost, privateKeysHost)
	return idp
}

// privateIPv4 is an RFC 1918 IPv4 address of this machine.
func privateIPv4(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && n.IP.IsPrivate() {
			return n.IP.To4()
		}
	}
	t.Skip("no RFC 1918 IPv4 address on this machine")
	return nil
}

func selfSignedFor(t *testing.T, hosts []string) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: hosts[0]},
		DNSNames:              hosts,
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

// bypassProxy clears the inherited proxy variables, then skips when the
// process-wide proxy setting, read once by net/http, still routes the test
// hosts through a proxy that cannot resolve them.
func bypassProxy(t *testing.T, hosts ...string) {
	t.Helper()
	for _, name := range []string{"HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
		t.Setenv(name, "")
	}
	for _, h := range hosts {
		req, err := http.NewRequest(http.MethodGet, "https://"+h+"/", nil)
		require.NoError(t, err)
		if proxy, _ := http.ProxyFromEnvironment(req); proxy != nil {
			t.Skipf("the process proxy setting routes %s through %s", h, proxy.Host)
		}
	}
}

// resolveTestHosts answers A queries for hosts with ip, through a DNS server
// on loopback that net.DefaultResolver uses for the rest of the test; every
// other name gets NXDOMAIN.
func resolveTestHosts(t *testing.T, ip net.IP, hosts ...string) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	known := map[string]bool{}
	for _, h := range hosts {
		known[h+"."] = true
	}
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			var parser dnsmessage.Parser
			header, err := parser.Start(buf[:n])
			if err != nil {
				continue
			}
			question, err := parser.Question()
			if err != nil {
				continue
			}
			reply := dnsmessage.Message{
				Header:    dnsmessage.Header{ID: header.ID, Response: true, Authoritative: true, RCode: dnsmessage.RCodeSuccess},
				Questions: []dnsmessage.Question{question},
			}
			name := strings.ToLower(question.Name.String())
			switch {
			case !known[name]:
				reply.RCode = dnsmessage.RCodeNameError
			case question.Type == dnsmessage.TypeA:
				var a [4]byte
				copy(a[:], ip.To4())
				reply.Answers = []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60},
					Body:   &dnsmessage.AResource{A: a},
				}}
			}
			packed, err := reply.Pack()
			if err == nil {
				_, _ = conn.WriteTo(packed, from)
			}
		}
	}()

	previous := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
	t.Cleanup(func() { net.DefaultResolver = previous })
}

func (p *privateIdP) idToken(t *testing.T, aud []string) string {
	t.Helper()
	return p.idTokenWith(t, aud, nil)
}

// idTokenWith signs an ID token with additional claims.
func (p *privateIdP) idTokenWith(t *testing.T, aud []string, additional map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: p.key},
		(&jose.SignerOptions{}).WithHeader("kid", privateTestKID).WithType("JWT"))
	require.NoError(t, err)
	claims := jwt.Claims{Issuer: p.issuer, Subject: "sub-admin", Audience: jwt.Audience(aud),
		Expiry: jwt.NewNumericDate(time.Now().Add(30 * time.Minute)), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}
	extra := map[string]any{"email": "admin@lab.local", "email_verified": true, "name": "Lab Admin", "groups": []string{"platform-admins"}}
	maps.Copy(extra, additional)
	tok, err := jwt.Signed(signer).Claims(claims).Claims(extra).Serialize()
	require.NoError(t, err)
	return tok
}

func (p *privateIdP) config(t *testing.T, jwksHosts ...string) OAuthConfig {
	t.Helper()
	environment := dexEnvironment(p.issuer, p.caFile)
	environment["OAUTH_ALLOW_PRIVATE_IP_JWKS_HOSTS"] = strings.Join(jwksHosts, ",")
	return OAuthConfig{Settings: oauthSettings(t, environment)}
}

// callThroughChain sends one request with bearer through the production
// newOAuth and protect chain and returns the status and the caller seen.
func callThroughChain(t *testing.T, cfg OAuthConfig, bearer string) (int, string) {
	t.Helper()
	o, err := newOAuth(cfg, "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		o.shutdown(ctx)
	})
	var caller string
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caller = identity.Caller(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, api.Prefix+"/models", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, caller
}

// TestForwardedTokenThroughTheProductionChain validates a forwarded id_token
// against a Dex on a private address with no private-address setting: the
// issuer host's JWKS is allowed by the library.
func TestForwardedTokenThroughTheProductionChain(t *testing.T) {
	idp := newPrivateIdP(t, "")

	code, caller := callThroughChain(t, idp.config(t), idp.idToken(t, []string{"agent-platform"}))
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, "admin@lab.local", caller)

	code, _ = callThroughChain(t, idp.config(t), idp.idToken(t, []string{"someone-else"}))
	require.Equal(t, http.StatusUnauthorized, code, "an untrusted audience is refused")

	code, _ = callThroughChain(t, idp.config(t), "")
	require.Equal(t, http.StatusUnauthorized, code, "no token, no API")
}

// TestJWKSOnAnotherPrivateHost pins the split-host case: a jwks_uri on a host
// other than the issuer's that resolves privately is refused until that host
// is listed in AllowPrivateIPJWKSHosts.
func TestJWKSOnAnotherPrivateHost(t *testing.T) {
	idp := newPrivateIdP(t, privateKeysHost)
	token := idp.idToken(t, []string{"agent-platform"})

	code, _ := callThroughChain(t, idp.config(t), token)
	require.Equal(t, http.StatusUnauthorized, code, "an unlisted private JWKS host is refused")

	code, caller := callThroughChain(t, idp.config(t, privateKeysHost), token)
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, "admin@lab.local", caller)
}

// trustedIssuerConfig declares the IdP as the OpenID Provider people sign in
// with (an OAUTH_TRUSTED_ISSUERS entry with openIDProvider) instead of
// trusting audiences of forwarded ID tokens, as the chart renders it.
func (p *privateIdP) trustedIssuerConfig(t *testing.T, downstream bool, jwksHosts ...string) OAuthConfig {
	t.Helper()
	environment := dexEnvironment(p.issuer, p.caFile)
	delete(environment, "OAUTH_TRUSTED_AUDIENCES")
	entry, err := json.Marshal([]map[string]any{{
		"issuer":                  p.issuer,
		"jwksURI":                 p.issuer + "/keys",
		"audiences":               []string{"agent-platform", "kubernetes"},
		"acceptedTypHeaders":      []string{"", "JWT"},
		"caFile":                  p.caFile,
		"allowPrivateIPJWKSHosts": jwksHosts,
		"openIDProvider":          true,
	}})
	require.NoError(t, err)
	environment["OAUTH_TRUSTED_ISSUERS"] = string(entry)
	return OAuthConfig{Settings: oauthSettings(t, environment), DownstreamOAuth: downstream}
}

// TestOpenIDProviderTrustedIssuer pins that the IdP as a trusted issuer
// marked openIDProvider admits what audience trust admits: the same caller,
// and with downstream OAuth the bearer itself as the token presented to the
// Kubernetes API.
func TestOpenIDProviderTrustedIssuer(t *testing.T) {
	idp := newPrivateIdP(t, "")
	token := idp.idToken(t, []string{"kubernetes"})

	code, _ := callThroughChain(t, idp.trustedIssuerConfig(t, false), token)
	require.Equal(t, http.StatusUnauthorized, code, "a private JWKS host must be listed")

	code, caller := callThroughChain(t, idp.trustedIssuerConfig(t, false, privateIssuerHost), token)
	require.Equal(t, http.StatusNoContent, code)
	require.Equal(t, "admin@lab.local", caller)

	code, _ = callThroughChain(t, idp.trustedIssuerConfig(t, false, privateIssuerHost), idp.idToken(t, []string{"someone-else"}))
	require.Equal(t, http.StatusUnauthorized, code, "an untrusted audience is refused")

	delegated := idp.idTokenWith(t, []string{"kubernetes"}, map[string]any{"act": map[string]any{"iss": idp.issuer, "sub": "agent"}})
	code, _ = callThroughChain(t, idp.trustedIssuerConfig(t, false, privateIssuerHost), delegated)
	require.Equal(t, http.StatusUnauthorized, code, "the provider's token carrying act is refused")
	code, _ = callThroughChain(t, idp.config(t), delegated)
	require.Equal(t, http.StatusUnauthorized, code, "as it is under trusted audiences")

	o, err := newOAuth(idp.trustedIssuerConfig(t, true, privateIssuerHost), "/mcp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()
		o.shutdown(ctx)
	})
	var presented string
	h := o.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented, _ = identity.TokenFromContext(r.Context())
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, api.Prefix+"/models", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Equal(t, token, presented, "the forwarded ID token is what the Kubernetes API sees")
}
