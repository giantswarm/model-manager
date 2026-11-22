package server

import (
	"context"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const dexProxyChildEnv = "MODEL_MANAGER_DEX_PROXY_CHILD"

func TestDexProviderFollowsProxyEnvironment(t *testing.T) {
	backend := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(backend.Close)

	tests := []struct {
		name        string
		noProxy     string
		wantTunnels []string
	}{
		{name: "issuer behind HTTPS_PROXY", wantTunnels: []string{"example.com:443"}},
		{name: "issuer in NO_PROXY", noProxy: "example.com", wantTunnels: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proxy := newTunnelProxy(t, backend.Listener.Addr().String())

			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], //nolint:gosec // re-executes this test binary
				"-test.run=^TestDexProviderProxyChild$", "-test.count=1", "-test.v")
			command.Env = append(environmentWithoutProxy(),
				dexProxyChildEnv+"=1",
				"HTTPS_PROXY="+proxy.server.URL,
				"NO_PROXY="+tc.noProxy,
			)
			output, _ := command.CombinedOutput()

			tunnels := proxy.connectedHosts()
			if tc.wantTunnels == nil {
				assert.Empty(t, tunnels, "child output:\n%s", output)
				return
			}
			require.NotEmpty(t, tunnels, "child output:\n%s", output)
			assert.Equal(t, tc.wantTunnels[0], tunnels[0], "child output:\n%s", output)
		})
	}
}

// TestDexProviderProxyChild runs only as the child process of
// TestDexProviderFollowsProxyEnvironment: net/http reads the proxy variables
// once per process, so each proxy setting needs a fresh process.
func TestDexProviderProxyChild(t *testing.T) {
	if os.Getenv(dexProxyChildEnv) != "1" {
		t.Skip("child process of TestDexProviderFollowsProxyEnvironment")
	}
	runtime, err := newOAuth(OAuthConfig{
		BaseURL:         "https://model-manager.example.com",
		Provider:        ProviderDex,
		DexIssuerURL:    "https://example.com",
		DexClientID:     "model-manager",
		DexClientSecret: "secret",
	}, "/mcp", slog.New(slog.DiscardHandler))
	if runtime != nil {
		t.Cleanup(func() { runtime.shutdown(context.WithoutCancel(t.Context())) })
	}
	t.Logf("OAuth setup: %v", err)
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

// tunnelProxy is an HTTPS proxy that records each CONNECT target and tunnels
// every one of them to a fixed backend address.
type tunnelProxy struct {
	server  *httptest.Server
	mutex   sync.Mutex
	targets []string
}

func newTunnelProxy(t *testing.T, backendAddress string) *tunnelProxy {
	t.Helper()
	proxy := &tunnelProxy{}
	proxy.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		proxy.mutex.Lock()
		proxy.targets = append(proxy.targets, r.Host)
		proxy.mutex.Unlock()

		backendConnection, err := net.Dial("tcp", backendAddress)
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
