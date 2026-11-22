package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/identity"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

func TestRESTAndMCPJobAuthorization(t *testing.T) {
	for _, transport := range []string{"rest", "mcp"} {
		t.Run(transport, func(t *testing.T) {
			lifetime, cancel := context.WithCancel(t.Context())
			defer cancel()
			manager := jobs.NewManager(jobs.WithLifetime(lifetime))
			t.Cleanup(func() {
				ctx, stop := context.WithTimeout(context.WithoutCancel(t.Context()), time.Second)
				defer stop()
				require.NoError(t, manager.Shutdown(ctx))
			})
			driver := newFakeBackend()
			driver.pullBlock = make(chan struct{})
			svc := service.New([]backend.Backend{driver}, manager, nil, nil, service.Config{}, nil)
			mux := http.NewServeMux()
			NewREST(svc, nil).Register(mux)
			mux.Handle("/mcp", mcpserver.NewStreamableHTTPServer(NewMCPServer(svc, buildinfo.Info{Version: "test"}), mcpserver.WithStateLess(true)))
			host := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				caller := &identity.Identity{Issuer: "https://issuer.example.test", Subject: request.Header.Get("X-Test-Subject"), ClientID: "client", Email: request.Header.Get("X-Test-Email")}
				mux.ServeHTTP(writer, request.WithContext(identity.ContextWith(request.Context(), caller)))
			}))
			t.Cleanup(host.Close)
			invoke := func(subject, operation, identifier string, dryRun ...bool) (map[string]any, bool) {
				t.Helper()
				var method, path, payload string
				if transport == "mcp" {
					method, path = http.MethodPost, "/mcp"
					arguments := map[string]any{}
					if len(dryRun) > 0 {
						arguments["dryRun"] = dryRun[0]
					}
					switch operation {
					case ToolPullModel:
						arguments["model"] = "shared-model"
					case ToolGetJob, ToolCancelJob:
						arguments["id"] = identifier
					}
					encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": operation, "arguments": arguments}})
					require.NoError(t, err)
					payload = string(encoded)
				} else {
					switch operation {
					case ToolPullModel:
						method, path, payload = http.MethodPost, Prefix+"/models/pull", `{"model":"shared-model"}`
					case ToolListJobs:
						method, path = http.MethodGet, Prefix+"/jobs"
					case ToolGetJob:
						method, path = http.MethodGet, Prefix+"/jobs/"+identifier
					case ToolCancelJob:
						method, path = http.MethodDelete, Prefix+"/jobs/"+identifier
					}
				}
				request, err := http.NewRequestWithContext(t.Context(), method, host.URL+path, strings.NewReader(payload))
				require.NoError(t, err)
				request.Header.Set("X-Test-Subject", subject)
				request.Header.Set("X-Test-Email", "same-display@example.test")
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("Accept", "application/json, text/event-stream")
				request.Header.Set("MCP-Protocol-Version", "2025-11-25")
				response, err := host.Client().Do(request)
				require.NoError(t, err)
				encoded, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				var body map[string]any
				require.NoError(t, json.Unmarshal(encoded, &body), string(encoded))
				if transport == "rest" {
					return body, response.StatusCode >= 400
				}
				result, ok := body["result"].(map[string]any)
				require.True(t, ok, string(encoded))
				failed, _ := result["isError"].(bool)
				text := result["content"].([]any)[0].(map[string]any)["text"].(string)
				if failed {
					return map[string]any{"message": text}, true
				}
				var decoded map[string]any
				require.NoError(t, json.Unmarshal([]byte(text), &decoded))
				return decoded, false
			}
			alice, failed := invoke("alice", ToolPullModel, "")
			require.False(t, failed, alice)
			aliceID := alice["job"].(map[string]any)["id"].(string)
			bob, failed := invoke("bob", ToolPullModel, "")
			require.False(t, failed, bob)
			bobID := bob["job"].(map[string]any)["id"].(string)
			require.NotEqual(t, aliceID, bobID)
			for _, operation := range []string{ToolGetJob, ToolCancelJob} {
				_, failed = invoke("bob", operation, aliceID)
				require.True(t, failed, operation)
			}
			if transport == "mcp" {
				_, failed = invoke("bob", ToolCancelJob, aliceID, true)
				require.True(t, failed)
			}
			visible, failed := invoke("bob", ToolListJobs, "")
			require.False(t, failed)
			list := visible["jobs"].([]any)
			require.Len(t, list, 1)
			require.Equal(t, bobID, list[0].(map[string]any)["id"])
			_, failed = invoke("alice", ToolCancelJob, aliceID)
			require.False(t, failed)
			_, failed = invoke("bob", ToolGetJob, bobID)
			require.False(t, failed)
		})
	}
}

func TestLoadRequiresTaskIdentityBeforeServingOrWiring(t *testing.T) {
	driver := newFakeServing()
	driver.models["org/tiny"] = backend.Model{Name: "org/tiny"}
	manager := jobs.NewManager()
	wirer := newFakeWirer()
	svc := service.New([]backend.Backend{driver}, manager, wirer, nil, service.Config{AutoWire: true}, nil)
	ctx := identity.ContextWith(t.Context(), &identity.Identity{Issuer: "https://issuer.example.test", Subject: "alice"})
	_, err := svc.Load(ctx, service.LoadOptions{Model: "org/tiny"})
	require.ErrorIs(t, err, jobs.ErrIdentityRequired)
	driver.fakeBackend.mu.Lock()
	defer driver.fakeBackend.mu.Unlock()
	require.Empty(t, driver.loaded)
	require.Empty(t, manager.List())
	require.Zero(t, wirer.count())
}
