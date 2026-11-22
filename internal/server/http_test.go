package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// downBackend is a backend whose upstream never answers.
type downBackend struct{}

func (downBackend) Name() backend.Name                 { return backend.NameOllama }
func (downBackend) Capabilities() backend.Capabilities { return backend.Capabilities{} }
func (downBackend) Info(context.Context) backend.Info {
	return backend.Info{Backend: backend.NameOllama, Healthy: false, Message: "connection refused"}
}
func (downBackend) ListModels(context.Context) ([]backend.Model, error) { return nil, nil }
func (downBackend) GetModel(context.Context, string) (*backend.Model, error) {
	return nil, backend.ErrNotFound
}
func (downBackend) ListLoaded(context.Context) ([]backend.LoadedModel, error) { return nil, nil }
func (downBackend) Pull(context.Context, backend.PullRequest, func(backend.Progress)) error {
	return backend.ErrUnsupported
}
func (downBackend) Delete(context.Context, string) error            { return backend.ErrUnsupported }
func (downBackend) Load(context.Context, backend.LoadRequest) error { return backend.ErrUnsupported }
func (downBackend) Unload(context.Context, string) error            { return backend.ErrUnsupported }
func (downBackend) AgentEndpoint(model string) backend.AgentEndpoint {
	return backend.AgentEndpoint{Provider: "Ollama", Model: model}
}

func TestReadinessDoesNotTrackBackend(t *testing.T) {
	svc := service.New([]backend.Backend{downBackend{}}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, svc, api.NewMCPServer(svc, buildinfo.Info{Version: "test"}), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	get := func(path string) int {
		resp, err := http.Get(ts.URL + path)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	assert.Equal(t, http.StatusOK, get("/healthz"))
	assert.Equal(t, http.StatusOK, get("/readyz"), "the API stays ready while the backend is down")
	assert.Equal(t, http.StatusServiceUnavailable, get("/backendz"), "the backend probe reports the outage")
	assert.Equal(t, http.StatusOK, get("/api/v1/backend"), "clients can still read healthy=false")
}

// A body beyond the bound is refused, and the endpoint keeps answering.
func TestMCPBodyIsBounded(t *testing.T) {
	svc := service.New([]backend.Backend{downBackend{}}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, svc, api.NewMCPServer(svc, buildinfo.Info{Version: "test"}), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	post := func(body string) int {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("x", maxMCPBodyBytes) + `"}}`
	assert.GreaterOrEqual(t, post(big), 400)
	assert.Equal(t, http.StatusOK, post(`{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`))
}

func TestMCPNativeStatelessCompatibilityAndOrigin(t *testing.T) {
	svc := service.New(nil, jobs.NewManager(), nil, nil, service.Config{}, nil)
	server, err := New(Config{MCPEnabled: true}, svc, api.NewMCPServer(svc, buildinfo.Info{Version: "test"}), nil)
	require.NoError(t, err)
	host := httptest.NewServer(server.Handler())
	defer host.Close()
	post := func(version, body string) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, host.URL+"/mcp", strings.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("MCP-Protocol-Version", version)
		response, err := host.Client().Do(request)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, response.Body.Close()) })
		return response
	}
	for _, version := range []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			initialized := post(version, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"`+version+`","capabilities":{},"clientInfo":{"name":"older-client","version":"test"}}}`)
			require.Equal(t, http.StatusOK, initialized.StatusCode)
			require.Empty(t, initialized.Header.Get("Mcp-Session-Id"))
			result := post(version, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_info","arguments":{}}}`)
			require.Equal(t, http.StatusOK, result.StatusCode)
			encoded, err := io.ReadAll(result.Body)
			require.NoError(t, err)
			require.Contains(t, string(encoded), "backends")
			require.NotContains(t, string(encoded), `"error":`)
		})
	}
	t.Run("2026-07-28", func(t *testing.T) {
		native, err := client.NewStreamableHttpClient(host.URL + "/mcp")
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, native.Close()) })
		require.NoError(t, native.Start(t.Context()))
		initialized, err := native.Initialize(t.Context(), mcp.InitializeRequest{})
		require.NoError(t, err)
		require.Equal(t, mcp.ProtocolVersion20260728, initialized.ProtocolVersion)
		discovered, err := native.Discover(t.Context(), mcp.DiscoverRequest{})
		require.NoError(t, err)
		require.Contains(t, discovered.SupportedVersions, mcp.ProtocolVersion20260728)
		request := mcp.CallToolRequest{}
		request.Params.Name = api.ToolGetInfo
		result, err := native.CallTool(t.Context(), request)
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.True(t, result.ResultType.IsComplete())
		require.Contains(t, result.Content[0].(mcp.TextContent).Text, "backends")
	})
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		request, err := http.NewRequestWithContext(t.Context(), method, host.URL+"/mcp", nil)
		require.NoError(t, err)
		request.Header.Set("Origin", "https://attacker.example")
		request.Header.Set("Sec-Fetch-Site", "same-origin")
		response, err := host.Client().Do(request)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusForbidden, response.StatusCode)
	}
}
