package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"

	oauth "github.com/giantswarm/mcp-oauth"
	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
	"github.com/giantswarm/mcp-oauth/providers/dex"
	"github.com/giantswarm/mcp-oauth/providers/google"
	"github.com/giantswarm/mcp-oauth/security"
	"github.com/giantswarm/mcp-oauth/storage/memory"

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
	// DexAllowPrivateIP lets the issuer resolve to a private/loopback address
	// (an in-cluster Dex).
	DexAllowPrivateIP bool

	// Google provider (Provider == google).
	GoogleClientID     string
	GoogleClientSecret string

	// TrustedAudiences are the OAuth client ids whose IdP tokens are accepted
	// (SSO token forwarding): the platform client muster and the portal log
	// in with. Empty: only tokens this server issued itself.
	TrustedAudiences []string
	// SSOAllowPrivateIPs lets the JWKS endpoint used for forwarded tokens
	// resolve to a private address (an in-cluster Dex).
	SSOAllowPrivateIPs bool
	// AllowPublicClientRegistration lets MCP clients register over DCR
	// without a token (labs only).
	AllowPublicClientRegistration bool
	// DownstreamOAuth puts the caller's IdP token on the request so
	// Kubernetes API calls are made as the caller instead of the
	// ServiceAccount (the apiserver must trust the IdP and the token's
	// audience). Background work keeps the ServiceAccount.
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
	store   *memory.Store
	cfg     OAuthConfig
	mcpPath string
	log     *slog.Logger
}

func newOAuth(cfg OAuthConfig, mcpPath string, log *slog.Logger) (*oauthRuntime, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	rootCAs, err := loadRootCAs(cfg.DexCAFile)
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	provider, err := newProvider(cfg, rootCAs, log)
	if err != nil {
		return nil, err
	}
	store := memory.New()
	serverCfg := &oauth.ServerConfig{
		Issuer:                        cfg.BaseURL,
		AllowRefreshTokenRotation:     true,
		AllowPublicClientRegistration: cfg.AllowPublicClientRegistration,
		MaxClientsPerIP:               10,
		TrustedAudiences:              cfg.TrustedAudiences,
		AllowPrivateIPJWKS:            cfg.SSOAllowPrivateIPs,
		JWKSRootCAs:                   rootCAs,
	}
	srv, err := oauth.NewServer(provider, store, store, store, serverCfg, log,
		oauth.WithAuditor(security.NewAuditor(log, true)))
	if err != nil {
		return nil, fmt.Errorf("oauth: server: %w", err)
	}
	log.Info("OAuth resource server enabled", "provider", cfg.provider(), "issuer", cfg.BaseURL,
		"trustedAudiences", cfg.TrustedAudiences, "downstreamOAuth", cfg.DownstreamOAuth)
	return &oauthRuntime{server: srv, handler: handler.New(srv, log), store: store, cfg: cfg, mcpPath: mcpPath, log: log}, nil
}

func newProvider(cfg OAuthConfig, rootCAs *x509.CertPool, log *slog.Logger) (providers.Provider, error) {
	redirect := strings.TrimSuffix(cfg.BaseURL, "/") + "/oauth/callback"
	switch cfg.provider() {
	case ProviderGoogle:
		p, err := google.NewProvider(&google.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			RedirectURL:  redirect,
		})
		if err != nil {
			return nil, fmt.Errorf("oauth: google provider: %w", err)
		}
		return p, nil
	default:
		p, err := dex.NewProvider(&dex.Config{
			IssuerURL:      cfg.DexIssuerURL,
			ClientID:       cfg.DexClientID,
			ClientSecret:   cfg.DexClientSecret,
			RedirectURL:    redirect,
			AllowPrivateIP: cfg.DexAllowPrivateIP,
			RootCAs:        rootCAs,
			Logger:         log,
		})
		if err != nil {
			return nil, fmt.Errorf("oauth: dex provider: %w", err)
		}
		return p, nil
	}
}

// loadRootCAs is the system pool plus the PEM bundle at caFile; (nil, nil)
// without a file, selecting the system trust everywhere the pool is used.
func loadRootCAs(caFile string) (*x509.CertPool, error) {
	if caFile == "" {
		return nil, nil
	}
	pem, err := os.ReadFile(caFile) // #nosec G304 -- operator-provided path
	if err != nil {
		return nil, fmt.Errorf("read CA file %s: %w", caFile, err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no CA certificate in %s", caFile)
	}
	return pool, nil
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

// protect requires a valid bearer token (mcp-oauth ValidateToken: this
// server's own access tokens, or a forwarded IdP id_token whose audience is
// trusted) and then attaches the caller to the request.
func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	return o.handler.ValidateToken(o.attachIdentity(next))
}

// attachIdentity translates the validated mcp-oauth user into the request's
// identity and, with DownstreamOAuth, resolves the IdP token to present to
// the Kubernetes API: a forwarded id_token is the bearer itself; for a token
// this server issued, the provider's id_token is looked up in the store.
func (o *oauthRuntime) attachIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		info, ok := handler.UserInfoFromContext(ctx)
		if !ok || info == nil {
			// ValidateToken admits nothing without user info; defensive.
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		id := &identity.Identity{Subject: info.ID, Email: info.Email, Name: info.Name, Groups: info.Groups, Source: identity.SourceOAuth}
		if info.IsSSO() {
			id.Source = identity.SourceSSO
		}
		ctx = identity.ContextWith(ctx, id)
		if o.cfg.DownstreamOAuth {
			if tok := o.downstreamToken(ctx, r, info); tok != "" {
				ctx = identity.ContextWithToken(ctx, tok)
			} else {
				o.log.Warn("no IdP token for downstream Kubernetes access; the request runs as the ServiceAccount", "caller", id.String(), "source", id.Source)
			}
		}
		o.log.Debug("authenticated request", "caller", id.String(), "source", id.Source, "path", r.URL.Path)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (o *oauthRuntime) downstreamToken(ctx context.Context, r *http.Request, info *providers.UserInfo) string {
	bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if bearer == "" {
		return ""
	}
	if info.IsSSO() {
		return bearer
	}
	tok, err := o.store.GetToken(ctx, bearer)
	if err != nil || tok == nil {
		return ""
	}
	// mcp-oauth keeps the provider's id_token as the oauth2 token extra.
	idToken, _ := tok.Extra("id_token").(string)
	return idToken
}

func (o *oauthRuntime) shutdown(ctx context.Context) {
	// Shutdown stops the stores it was given.
	if err := o.server.Shutdown(ctx); err != nil {
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
