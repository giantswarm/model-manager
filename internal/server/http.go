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

	mcpserver "github.com/mark3labs/mcp-go/server"

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
	http  *http.Server
	oauth *oauthRuntime
	log   *slog.Logger
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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	// Readiness deliberately does not track the serving backend: when the host
	// Ollama is down the API must stay reachable so clients can render the
	// backend's health from GET /api/v1/backend (healthy=false) instead of
	// getting connection errors from an unready Service.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /backendz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := svc.Ready(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	s := &Server{log: log}
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
		mux.Handle(cfg.MCPPath, s.guard(mcpserver.NewStreamableHTTPServer(mcpSrv,
			mcpserver.WithEndpointPath(cfg.MCPPath),
		)))
	}

	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: MCP streams and long pulls outlive any fixed value.
		IdleTimeout: 120 * time.Second,
	}
	return s, nil
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
	errCh := make(chan error, 1)
	go func() {
		s.log.Info("listening", "addr", s.http.Addr)
		if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()
	select {
	case err := <-errCh:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if s.oauth != nil {
		s.oauth.shutdown(shutdownCtx)
	}
	if err := s.http.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	s.log.Info("server stopped")
	return nil
}
