package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers/dex"
	"github.com/giantswarm/mcp-oauth/storage/memory"
)

// OAuthConfig enables an embedded OAuth 2.1 authorization server (Dex
// upstream) in front of the MCP endpoint. Ported from llm-testing; optional.
type OAuthConfig struct {
	// BaseURL is the public URL clients use to reach this server.
	BaseURL         string
	DexIssuerURL    string
	DexClientID     string
	DexClientSecret string
}

// Validate checks required fields.
func (c OAuthConfig) Validate() error {
	if c.BaseURL == "" {
		return fmt.Errorf("oauth: base URL is required")
	}
	if err := validateHTTPS(c.BaseURL); err != nil {
		return err
	}
	if c.DexIssuerURL == "" || c.DexClientID == "" || c.DexClientSecret == "" {
		return fmt.Errorf("oauth: dex issuer URL, client ID and client secret are required")
	}
	return nil
}

type oauthRuntime struct {
	server  *oauth.Server
	handler *handler.Handler
	mcpPath string
}

func newOAuth(cfg OAuthConfig, mcpPath string, log *slog.Logger) (*oauthRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	provider, err := dex.NewProvider(&dex.Config{
		IssuerURL:    cfg.DexIssuerURL,
		ClientID:     cfg.DexClientID,
		ClientSecret: cfg.DexClientSecret,
		RedirectURL:  cfg.BaseURL + "/oauth/callback",
	})
	if err != nil {
		return nil, fmt.Errorf("oauth: dex provider: %w", err)
	}
	store := memory.New()
	srv, err := oauth.NewServer(provider, store, store, store, &oauth.ServerConfig{
		Issuer:                    cfg.BaseURL,
		AllowRefreshTokenRotation: true,
		MaxClientsPerIP:           10,
	}, log)
	if err != nil {
		return nil, fmt.Errorf("oauth: server: %w", err)
	}
	return &oauthRuntime{server: srv, handler: handler.New(srv, log), mcpPath: mcpPath}, nil
}

func (o *oauthRuntime) register(mux *http.ServeMux) {
	o.handler.RegisterAuthorizationServerMetadataRoutes(mux)
	o.handler.RegisterProtectedResourceMetadataRoutes(mux, o.mcpPath)
	mux.HandleFunc("/oauth/authorize", o.handler.ServeAuthorization)
	mux.HandleFunc("/oauth/token", o.handler.ServeToken)
	mux.HandleFunc("/oauth/callback", o.handler.ServeCallback)
	mux.HandleFunc("/oauth/register", o.handler.ServeClientRegistration)
	mux.HandleFunc("/oauth/revoke", o.handler.ServeTokenRevocation)
	mux.HandleFunc("/oauth/introspect", o.handler.ServeTokenIntrospection)
}

func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	return o.handler.ValidateToken(next)
}

func (o *oauthRuntime) shutdown(ctx context.Context) {
	if err := o.server.Shutdown(ctx); err != nil {
		slog.Warn("oauth server shutdown", "error", err)
	}
}

// validateHTTPS enforces OAuth 2.1's HTTPS requirement, allowing plain HTTP
// only for loopback development.
func validateHTTPS(baseURL string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("oauth: invalid base URL: %w", err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" || host == "::1" {
			return nil
		}
		return fmt.Errorf("oauth: base URL must use https (http is allowed for loopback only): %s", baseURL)
	default:
		return fmt.Errorf("oauth: base URL scheme must be http or https: %s", baseURL)
	}
}
