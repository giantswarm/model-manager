package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"strings"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/security"
	oauthserver "github.com/giantswarm/mcp-oauth/server"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/idtoken"
)

// OAuthConfig makes model-manager an OAuth 2.1 resource server (mcp-oauth) in
// front of the MCP endpoint and the REST API. The OpenID Provider people sign
// in with is the authority: on the Agent Platform, muster forwards the
// session's ID token byte-identical (MCPServer auth.forwardToken) and the
// portal sends the signed-in user's ID token through the gateway; both are
// validated against the provider's JWKS as a trusted issuer
// (OAUTH_TRUSTED_ISSUERS). The user mcp-oauth validated (providers.UserInfo)
// then travels with the request and, with DownstreamOAuth, so does their ID
// token: the Kubernetes API is called as the user.
type OAuthConfig struct {
	// Settings is mcp-oauth's configuration, read from the OAUTH_* variables
	// by oauthconfig.SettingsFromEnv.
	Settings oauthconfig.Settings
	// DownstreamOAuth puts the user's ID token on the request so Kubernetes
	// API calls are made as the user instead of the ServiceAccount (the
	// apiserver must trust the provider and the token's audience). The
	// ServiceAccount holds no permissions then, so a request that yields no
	// ID token to present is refused (401).
	DownstreamOAuth bool
}

type oauthRuntime struct {
	server          *oauthserver.Server
	handler         *handler.Handler
	cleanup         func(context.Context) error
	downstreamOAuth bool
	mcpPath         string
	log             *slog.Logger
}

func newOAuth(cfg OAuthConfig, mcpPath string, log *slog.Logger) (*oauthRuntime, error) {
	settings := cfg.Settings
	if settings.Server == nil {
		return nil, errors.New("oauth: no server settings")
	}
	serverConfig := *settings.Server
	serverConfig.ResourceMetadataByPath = protectedResources(&serverConfig, mcpPath)
	settings.Server = &serverConfig
	settings.Logger = log
	if settings.Dex != nil {
		dexConfig := *settings.Dex
		dexConfig.Logger = log
		settings.Dex = &dexConfig
	}
	runtime, cleanup, err := oauthconfig.Setup(settings, oauthserver.WithAuditor(security.NewAuditor(log, true)))
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	log.Info("OAuth resource server enabled", "provider", providerName(settings), "issuer", serverConfig.Issuer,
		"trustedAudiences", serverConfig.TrustedAudiences, "trustedIssuers", len(settings.TrustedIssuers),
		"downstreamOAuth", cfg.DownstreamOAuth)
	return &oauthRuntime{server: runtime.Server, handler: runtime.Handler, cleanup: cleanup,
		downstreamOAuth: cfg.DownstreamOAuth, mcpPath: mcpPath, log: log}, nil
}

// protectedResources adds the REST API and the MCP endpoint to the protected
// resources the configuration names. A path the configuration already
// describes keeps its entry; an added one advertises the main resource's
// authorization servers, as OAUTH_ADDITIONAL_RESOURCES entries do.
func protectedResources(configuration *oauthserver.Config, mcpPath string) map[string]oauthserver.ProtectedResourceConfig {
	resources := maps.Clone(configuration.ResourceMetadataByPath)
	if resources == nil {
		resources = map[string]oauthserver.ProtectedResourceConfig{}
	}
	// Metadata lookup ignores a trailing slash, so "/mcp/" describes "/mcp".
	described := func(path string) (oauthserver.ProtectedResourceConfig, bool) {
		for key, resource := range resources {
			if strings.TrimRight(key, "/") == strings.TrimRight(path, "/") {
				return resource, true
			}
		}
		return oauthserver.ProtectedResourceConfig{}, false
	}
	var advertised []string
	if main, err := url.Parse(configuration.GetResourceIdentifier()); err == nil {
		resource, _ := described(main.EscapedPath())
		advertised = resource.AuthorizationServers
	}
	for _, path := range []string{api.Prefix, mcpPath} {
		if path == "" || path == "/" {
			continue
		}
		if _, ok := described(path); !ok {
			resources[path] = oauthserver.ProtectedResourceConfig{AuthorizationServers: advertised}
		}
	}
	return resources
}

func providerName(settings oauthconfig.Settings) string {
	switch {
	case settings.Dex != nil:
		return "dex"
	case settings.Google != nil:
		return "google"
	default:
		return "none"
	}
}

func (o *oauthRuntime) register(mux *http.ServeMux) {
	o.handler.RegisterOAuthRoutes(mux, handler.OAuthRoutesOptions{MCPPath: o.mcpPath, IncludeMetadata: true})
}

// protect requires a valid bearer token (mcp-oauth ValidateToken: this
// server's own access tokens, or a forwarded ID token whose audience is
// trusted), which puts the validated user on the request. With
// DownstreamOAuth the request also needs the user's ID token
// (RequireProviderIDToken), which attachIDToken hands to the Kubernetes
// clients; the ServiceAccount then holds no permissions, so there is nothing
// else to run as.
func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	protected := next
	if o.downstreamOAuth {
		protected = o.handler.RequireProviderIDToken(attachIDToken(next))
	}
	return o.handler.ValidateToken(protected)
}

// attachIDToken puts the user's ID token on the request for the Kubernetes
// clients: a forwarded ID token is the bearer itself; for a token this server
// issued, the provider's ID token stored with it. A request without one is
// refused.
func attachIDToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, err := handler.ProviderIDTokenFromContext(r.Context())
		if err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(idtoken.ContextWith(r.Context(), token)))
	})
}

func (o *oauthRuntime) shutdown(ctx context.Context) {
	if err := o.cleanup(ctx); err != nil {
		o.log.Warn("oauth server shutdown", "error", err)
	}
}
