package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const dexProxyChildEnv = "MODEL_MANAGER_DEX_PROXY_CHILD"

func TestDexProviderFollowsProxyEnvironment(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], //nolint:gosec // re-executes this test binary
		"-test.run=^TestDexProviderProxyChild$", "-test.count=1", "-test.v")
	command.Env = append(environmentWithoutProxy(), dexProxyChildEnv+"=1")
	output, err := command.CombinedOutput()
	require.NoError(t, err, "%s", output)
}

// net/http caches proxy environment values, so this case needs its own process.
func TestDexProviderProxyChild(t *testing.T) {
	if os.Getenv(dexProxyChildEnv) != "1" {
		t.Skip("child process of TestDexProviderFollowsProxyEnvironment")
	}
	const hostname = "dex.example.test"
	var issuer string
	var requests int
	var requestsMutex sync.Mutex
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/dex/.well-known/openid-configuration" {
			http.NotFound(w, request)
			return
		}
		requestsMutex.Lock()
		requests++
		requestsMutex.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"userinfo_endpoint":                     issuer + "/userinfo",
			"jwks_uri":                              issuer + "/keys",
			"response_types_supported":              []string{"code"},
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	certificate := selfSignedServerCertificate(t, hostname)
	backend.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	_, port, err := net.SplitHostPort(backend.Listener.Addr().String())
	require.NoError(t, err)
	issuer = "https://" + net.JoinHostPort(hostname, port) + "/dex"
	backend.StartTLS()
	t.Cleanup(backend.Close)
	parsedCertificate, err := x509.ParseCertificate(certificate.Certificate[0])
	require.NoError(t, err)
	rootCAs := x509.NewCertPool()
	rootCAs.AddCert(parsedCertificate)
	proxy := newTunnelProxy(t, backend.Listener.Addr().String())
	t.Setenv("HTTPS_PROXY", proxy.server.URL)
	t.Setenv("NO_PROXY", "")

	provider, err := newProvider(OAuthConfig{
		BaseURL:         "https://model-manager.example.com",
		DexIssuerURL:    issuer,
		DexClientID:     "model-manager",
		DexClientSecret: "test-client-secret",
	}, rootCAs, slog.New(slog.DiscardHandler))
	require.NoError(t, err)
	require.NotNil(t, provider)
	require.Equal(t, []string{net.JoinHostPort(hostname, port)}, proxy.connectedHosts())
	requestsMutex.Lock()
	defer requestsMutex.Unlock()
	require.Equal(t, 1, requests)
}

func environmentWithoutProxy() []string {
	var environment []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasSuffix(strings.ToUpper(name), "_PROXY") {
			continue
		}
		environment = append(environment, entry)
	}
	return environment
}

type tunnelProxy struct {
	server  *httptest.Server
	mutex   sync.Mutex
	targets []string
}

func newTunnelProxy(t *testing.T, backendAddress string) *tunnelProxy {
	t.Helper()
	proxy := &tunnelProxy{}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		proxy.mutex.Lock()
		proxy.targets = append(proxy.targets, request.Host)
		proxy.mutex.Unlock()
		backendConnection, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", backendAddress)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			_ = backendConnection.Close()
			http.Error(w, "hijacking unsupported", http.StatusInternalServerError)
			return
		}
		clientConnection, _, err := hijacker.Hijack()
		if err != nil {
			_ = backendConnection.Close()
			return
		}
		_, _ = clientConnection.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n"))
		go func() {
			_, _ = io.Copy(backendConnection, clientConnection)
			_ = backendConnection.Close()
		}()
		_, _ = io.Copy(clientConnection, backendConnection)
		_ = clientConnection.Close()
	}))
	t.Cleanup(proxy.server.Close)
	return proxy
}

func (proxy *tunnelProxy) connectedHosts() []string {
	proxy.mutex.Lock()
	defer proxy.mutex.Unlock()
	return append([]string(nil), proxy.targets...)
}
