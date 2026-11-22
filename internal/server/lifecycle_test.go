package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

func TestAuthenticatedRequestsAndDrain(t *testing.T) {
	idp := newPrivateIdP(t, "")
	configuration := idp.config()
	svc := service.New([]backend.Backend{downBackend{}}, jobs.NewManager(jobs.WithAnonymousAccess(true)), nil, nil, service.Config{}, nil)
	server, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true, OAuth: &configuration}, svc, api.NewMCPServer(svc, buildinfo.Info{Version: "test"}), nil)
	require.NoError(t, err)
	addresses := make(chan string, 1)
	server.http.BaseContext = func(listener net.Listener) context.Context {
		addresses <- listener.Addr().String()
		return t.Context()
	}
	entered, release := make(chan struct{}), make(chan struct{})
	original := server.http.Handler
	server.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Test-Drain") != "" {
			close(entered)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		original.ServeHTTP(w, request)
	})
	servingContext, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Run(servingContext) }()
	t.Cleanup(func() { cancel(); close(release); _ = server.http.Close() })
	var address string
	select {
	case address = <-addresses:
	case <-time.After(5 * time.Second):
		t.Fatal("listener did not start")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	call := func(method, path, token, body string) (*http.Response, string) {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, "http://"+address+path, strings.NewReader(body))
		require.NoError(t, err)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		if method == http.MethodPost {
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", "application/json, text/event-stream")
		}
		response, err := client.Do(request)
		require.NoError(t, err)
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		return response, string(data)
	}
	for _, path := range []string{api.Prefix + "/backend", "/mcp"} {
		response, _ := call(http.MethodGet, path, "", "")
		require.Equal(t, http.StatusUnauthorized, response.StatusCode)
		require.Contains(t, response.Header.Get("WWW-Authenticate"), "resource_metadata=")
	}
	response, body := call(http.MethodGet, "/.well-known/oauth-protected-resource/mcp", "", "")
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, body, "authorization_servers")
	token := idp.idToken(t, []string{"agent-platform"})
	response, _ = call(http.MethodGet, api.Prefix+"/backend", token, "")
	require.Equal(t, http.StatusOK, response.StatusCode)
	response, body = call(http.MethodPost, "/mcp", token, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"pilot-test","version":"1"}}}`)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Contains(t, body, "protocolVersion")
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+api.Prefix+"/backend", nil)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("X-Test-Drain", "true")
	statuses := make(chan int, 1)
	go func() {
		response, err := client.Do(request)
		if err != nil {
			statuses <- 0
			return
		}
		defer func() { _ = response.Body.Close() }()
		_, _ = io.Copy(io.Discard, response.Body)
		statuses <- response.StatusCode
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter handler")
	}
	cancel()
	require.Eventually(t, func() bool { return !server.health.IsReady() }, time.Second, time.Millisecond)
	probe := httptest.NewRecorder()
	server.health.Readiness(probe, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	require.Equal(t, http.StatusServiceUnavailable, probe.Code)
	select {
	case err := <-done:
		t.Fatalf("returned before request drained: %v", err)
	default:
	}
	release <- struct{}{}
	select {
	case status := <-statuses:
		require.Equal(t, http.StatusOK, status)
	case <-time.After(5 * time.Second):
		t.Fatal("request failed to drain")
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown failed")
	}
}

func TestSharedShutdownDeadlineClosesRequestsBeforeOAuthCleanup(t *testing.T) {
	configuration := newPrivateIdP(t, "").config()
	svc := service.New(nil, jobs.NewManager(jobs.WithAnonymousAccess(true)), nil, nil, service.Config{}, nil)
	var once sync.Once
	var shutdownContext context.Context
	factory := func() context.Context {
		once.Do(func() {
			var cancel context.CancelFunc
			shutdownContext, cancel = context.WithTimeout(t.Context(), 50*time.Millisecond)
			t.Cleanup(cancel)
		})
		return shutdownContext
	}
	instance, err := New(Config{Addr: "127.0.0.1:0", OAuth: &configuration, ShutdownContext: factory}, svc, nil, nil)
	require.NoError(t, err)
	addresses := make(chan string, 1)
	instance.http.BaseContext = func(listener net.Listener) context.Context { addresses <- listener.Addr().String(); return t.Context() }
	entered, disconnected := make(chan struct{}), make(chan struct{})
	instance.http.Handler = http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(entered)
		<-request.Context().Done()
		close(disconnected)
	})
	cleanup := instance.oauth.cleanup
	var cleanupDeadline time.Time
	instance.oauth.cleanup = func(ctx context.Context) error {
		cleanupDeadline, _ = ctx.Deadline()
		return cleanup(ctx)
	}
	serving, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- instance.Run(serving) }()
	t.Cleanup(func() {
		cancel()
		_ = instance.http.Close()
	})
	var address string
	select {
	case address = <-addresses:
	case <-time.After(time.Second):
		t.Fatal("listener did not start")
	}
	responseDone := make(chan struct{})
	go func() {
		defer close(responseDone)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+address+"/work", nil)
		if err != nil {
			return
		}
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(2 * time.Second):
		t.Fatal("process deadline was extended")
	}
	require.False(t, instance.health.IsReady())
	deadline, _ := shutdownContext.Deadline()
	require.Equal(t, deadline, cleanupDeadline)
	select {
	case <-disconnected:
	case <-time.After(time.Second):
		t.Fatal("deadline left the request connected")
	}
	select {
	case <-responseDone:
	case <-time.After(time.Second):
		t.Fatal("client remained connected")
	}
}
