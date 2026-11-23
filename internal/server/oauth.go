package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/providers/dex"
	"github.com/giantswarm/mcp-oauth/providers/google"
	"github.com/giantswarm/mcp-oauth/security"
	oauthserver "github.com/giantswarm/mcp-oauth/server"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/identity"
)

// OAuth providers.
const (
	ProviderDex    = "dex"
	ProviderGoogle = "google"
)

// OAuthConfig makes model-manager an OAuth 2.1 resource server (mcp-oauth) in
// front of the MCP endpoint and the REST API. The platform's identity
// provider is the authority: on the Agent Platform, muster forwards the
// session's IdP id_token byte-identical (MCPServer auth.forwardToken) and the
// portal sends the signed-in user's id_token through the gateway; both are
// validated here against the IdP's JWKS when their audience is one of
// TrustedAudiences (the platform's OAuth client). The caller's identity then
// travels with the request (identity package) and, with DownstreamOAuth, so
// does the token — the Kubernetes API is called as the caller.
type OAuthConfig struct {
	// BaseURL is the public URL of this server: the OAuth issuer identifier
	// of its own authorization-server metadata (https, or http on loopback).
	BaseURL string
	// Provider is the IdP: dex (default) or google.
	Provider string

	// Dex provider (Provider == dex).
	DexIssuerURL    string
	DexClientID     string
	DexClientSecret string
	// DexCAFile is a PEM CA bundle for an IdP with a private certificate;
	// verifies discovery, token and JWKS calls. Empty: system trust.
	DexCAFile string

	// Google provider (Provider == google).
	GoogleClientID     string
	GoogleClientSecret string

	// TrustedAudiences are the OAuth client ids whose IdP id_tokens are
	// accepted as bearer tokens (SSO token forwarding): the platform client
	// MCP clients and the muster CLI log in with, plus the audiences the
	// MCPServer requires — every forwarded token carries those and the
	// kube-apiserver trusts them (the chart passes both). Empty: only tokens
	// this server issued itself.
	TrustedAudiences []string
	// AllowPrivateIPJWKSHosts lists the hosts, other than the issuer's own,
	// whose JWKS may resolve to RFC 1918 and RFC 4193 addresses: a jwks_uri
	// the IdP's discovery document puts on another host. The issuer host needs
	// no entry. Loopback, link-local and metadata addresses stay refused.
	AllowPrivateIPJWKSHosts []string
	// AllowPublicClientRegistration lets MCP clients register over DCR
	// without a token (labs only).
	AllowPublicClientRegistration bool
	// DownstreamOAuth puts the caller's IdP token on the request so
	// Kubernetes API calls are made as the caller instead of the
	// ServiceAccount (the apiserver must trust the IdP and the token's
	// audience). The ServiceAccount holds no permissions then, so a request
	// that yields no IdP token to present is refused (401).
	DownstreamOAuth bool
}

// Validate checks required fields.
func (c OAuthConfig) Validate() error {
	if c.BaseURL == "" {
		return fmt.Errorf("oauth: base URL is required")
	}
	if err := validateHTTPS(c.BaseURL); err != nil {
		return err
	}
	switch c.provider() {
	case ProviderDex:
		if c.DexIssuerURL == "" || c.DexClientID == "" || c.DexClientSecret == "" {
			return fmt.Errorf("oauth: dex issuer URL, client ID and client secret are required")
		}
	case ProviderGoogle:
		if c.GoogleClientID == "" || c.GoogleClientSecret == "" {
			return fmt.Errorf("oauth: google client ID and client secret are required")
		}
	default:
		return fmt.Errorf("oauth: provider %q: want %s or %s", c.Provider, ProviderDex, ProviderGoogle)
	}
	for _, a := range c.TrustedAudiences {
		if strings.TrimSpace(a) == "" {
			return fmt.Errorf("oauth: trusted audiences must not be empty strings")
		}
	}
	return nil
}

func (c OAuthConfig) provider() string {
	if c.Provider == "" {
		return ProviderDex
	}
	return c.Provider
}

type oauthRuntime struct {
	server  *oauth.Server
	handler *handler.Handler
	cleanup func(context.Context) error
	cfg     OAuthConfig
	mcpPath string
	log     *slog.Logger
}

func newOAuth(cfg OAuthConfig, mcpPath string, log *slog.Logger) (*oauthRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	redirect := strings.TrimSuffix(cfg.BaseURL, "/") + "/oauth/callback"
	settings := oauthconfig.Settings{Server: serverConfig(cfg, nil), RootCAFile: cfg.DexCAFile, Logger: log}
	settings.Server.ResourceMetadataByPath = map[string]oauthserver.ProtectedResourceConfig{api.Prefix: {}}
	if mcpPath != "" && mcpPath != "/" {
		settings.Server.ResourceMetadataByPath[mcpPath] = oauthserver.ProtectedResourceConfig{}
	}
	if cfg.provider() == ProviderGoogle {
		settings.Google = &google.Config{ClientID: cfg.GoogleClientID, ClientSecret: cfg.GoogleClientSecret, RedirectURL: redirect}
	} else {
		settings.Dex = &dex.Config{IssuerURL: cfg.DexIssuerURL, ClientID: cfg.DexClientID, ClientSecret: cfg.DexClientSecret, RedirectURL: redirect, Logger: log}
	}
	runtime, cleanup, err := oauthconfig.Setup(settings,
		oauth.WithAuditor(security.NewAuditor(log, true)))
	if err != nil {
		return nil, fmt.Errorf("oauth: server: %w", err)
	}
	log.Info("OAuth resource server enabled", "provider", cfg.provider(), "issuer", cfg.BaseURL,
		"trustedAudiences", cfg.TrustedAudiences, "downstreamOAuth", cfg.DownstreamOAuth)
	return &oauthRuntime{server: runtime.Server, handler: runtime.Handler, cleanup: cleanup, cfg: cfg, mcpPath: mcpPath, log: log}, nil
}

// serverConfig is the mcp-oauth server configuration for cfg: this server's
// own issuer, the audiences it trusts as forwarded bearers and the CA bundle
// for the issuer's JWKS.
func serverConfig(cfg OAuthConfig, rootCAs *x509.CertPool) *oauth.ServerConfig {
	return &oauth.ServerConfig{
		EnableClientIDMetadataDocuments: true,
		Issuer:                          cfg.BaseURL,
		AllowRefreshTokenRotation:       true,
		AllowPublicClientRegistration:   cfg.AllowPublicClientRegistration,
		MaxClientsPerIP:                 10,
		TrustedAudiences:                cfg.TrustedAudiences,
		AllowPrivateIPJWKSHosts:         cfg.AllowPrivateIPJWKSHosts,
		JWKSRootCAs:                     rootCAs,
	}
}

func (o *oauthRuntime) register(mux *http.ServeMux) {
	o.handler.RegisterOAuthRoutes(mux, handler.OAuthRoutesOptions{MCPPath: o.mcpPath, IncludeMetadata: true})
}

// protect requires a valid bearer token (mcp-oauth ValidateToken: this
// server's own access tokens, or a forwarded IdP id_token whose audience is
// trusted) and then attaches the caller to the request.
func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	protected := o.attachIdentity(next)
	if o.cfg.DownstreamOAuth {
		protected = o.handler.RequireProviderIDToken(protected)
	}
	return o.handler.ValidateToken(protected)
}

// attachIdentity translates the validated mcp-oauth user into the request's
// identity and, with DownstreamOAuth, resolves the IdP token to present to
// the Kubernetes API: a forwarded id_token is the bearer itself; for a token
// this server issued, the provider's id_token is looked up in the store. A
// request that yields neither is refused: the ServiceAccount holds no
// permissions, so there is nothing else to run as.
func (o *oauthRuntime) attachIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		info, ok := handler.UserInfoFromContext(ctx)
		if !ok || info == nil || info.Issuer == "" || info.ID == "" || (info.ActorIssuer == "") != (info.ActorSubject == "") {
			// ValidateToken admits nothing without user info; defensive.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := &identity.Identity{Issuer: info.Issuer, Subject: info.ID, ActorIssuer: info.ActorIssuer, ActorSubject: info.ActorSubject, Email: info.Email, Name: info.Name, Groups: info.Groups, Source: identity.SourceOAuth}
		id.ClientID = info.ClientID
		id.Scopes, _ = handler.ScopesFromContext(ctx)
		if len(id.Scopes) == 0 {
			id.Scopes = info.Scopes
		}
		if info.IsSSO() {
			id.Source = identity.SourceSSO
		}
		ctx = identity.ContextWith(ctx, id)
		if o.cfg.DownstreamOAuth {
			token, err := handler.ProviderIDTokenFromContext(ctx)
			if err != nil {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx = identity.ContextWithToken(ctx, token)
		}
		o.log.Debug("authenticated request", identity.LogAttr(ctx), "source", id.Source, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (o *oauthRuntime) shutdown(ctx context.Context) {
	if err := o.cleanup(ctx); err != nil {
		o.log.Warn("oauth server shutdown", "error", err)
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
