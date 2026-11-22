// Package server assembles the single HTTP listener: health endpoints, the
// REST API, and the MCP streamable-HTTP endpoint (optionally behind OAuth).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/giantswarm/mcp-toolkit/health"
	"github.com/giantswarm/mcp-toolkit/httpx"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/service"
)

// Config configures the listener.
type Config struct {
	Addr       string
	MCPEnabled bool
	MCPPath    string
	// OAuth, when set, makes the server an OAuth 2.1 resource server: the MCP
	// endpoint and the REST API require a bearer token the platform IdP
	// issued (forwarded by muster / sent by the portal) or this server's own,
	// and every call carries the caller's identity. Off: anonymous, acting as
	// the ServiceAccount — only for a server nothing but a trusted proxy can
	// reach.
	OAuth *OAuthConfig
}

// Server is the assembled HTTP server.
type Server struct {
	http   *http.Server
	oauth  *oauthRuntime
	log    *slog.Logger
	health *health.Health
}

// New builds the server.
func New(cfg Config, svc *service.Service, mcpSrv *mcpserver.MCPServer, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	if cfg.MCPPath == "" {
		cfg.MCPPath = "/mcp"
	}
	mux := http.NewServeMux()
	healthState := health.New()
	healthState.Mount(mux)
	mux.HandleFunc("GET /backendz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := svc.Ready(ctx); err != nil {
			http.Error(w, "backend unavailable", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s := &Server{log: log, health: healthState}
	if cfg.OAuth != nil {
		o, err := newOAuth(*cfg.OAuth, cfg.MCPPath, log)
		if err != nil {
			return nil, err
		}
		o.register(mux)
		s.oauth = o
	}

	// The REST API on its own mux so one guard covers every route; the
	// health endpoints above stay open for the probes.
	rest := http.NewServeMux()
	api.NewREST(svc, log).Register(rest)
	mux.Handle(api.Prefix+"/", s.guard(rest))

	if cfg.MCPEnabled && mcpSrv != nil {
		// mcp-go reads a POST body whole before validating it
		// (CVE-2026-108859): the bound keeps that read to what the REST API accepts.
		mcpHandler := mcpserver.NewStreamableHTTPServer(mcpSrv, mcpserver.WithEndpointPath(cfg.MCPPath), mcpserver.WithStateLess(true))
		mux.Handle(cfg.MCPPath, httpx.ProtectOrigin(http.MaxBytesHandler(s.guard(mcpHandler), maxMCPBodyBytes)))
	}

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           otelhttp.NewHandler(mux, "model-manager", otelhttp.WithFilter(traced)),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams and long pulls outlive any fixed value.
		IdleTimeout: 120 * time.Second,
	}
	healthState.SetReady(true)
	return s, nil
}

// traced leaves the kubelet probes out of the traces.
// maxMCPBodyBytes bounds an MCP request body, as the REST API bounds its own.
const maxMCPBodyBytes = 1 << 20

func traced(r *http.Request) bool {
	return r.URL.Path != "/healthz" && r.URL.Path != "/readyz"
}

// guard requires an authenticated caller when OAuth is on.
func (s *Server) guard(next http.Handler) http.Handler {
	if s.oauth == nil {
		return next
	}
	return s.oauth.protect(next)
}

// Handler exposes the mux (tests).
func (s *Server) Handler() http.Handler { return s.http.Handler }

// Run serves until ctx is done, then shuts down gracefully.
func (s *Server) Run(ctx context.Context) error {
	servingContext, cancelServing := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelServing()
	stop := context.AfterFunc(ctx, func() {
		s.health.SetReady(false)
		cancelServing()
	})
	defer stop()
	s.log.Info("listening", "addr", s.http.Addr)
	err := httpx.Run(servingContext, s.http, 15*time.Second)
	s.health.SetReady(false)
	if errors.Is(err, context.DeadlineExceeded) {
		err = errors.Join(err, s.http.Close())
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if s.oauth != nil {
		s.oauth.shutdown(shutdownCtx)
	}
	if err != nil {
		return fmt.Errorf("http server: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}
