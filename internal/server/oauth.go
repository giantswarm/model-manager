package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/oauthconfig"
	"github.com/giantswarm/mcp-oauth/security"
	oauthserver "github.com/giantswarm/mcp-oauth/server"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/identity"
)

// OAuthConfig makes model-manager an OAuth 2.1 resource server (mcp-oauth) in
// front of the MCP endpoint and the REST API. The platform's identity
// provider is the authority: on the Agent Platform, muster forwards the
// session's IdP id_token byte-identical (MCPServer auth.forwardToken) and the
// portal sends the signed-in user's id_token through the gateway; both are
// validated here against the IdP's JWKS when their audience is one of the
// trusted audiences (OAUTH_TRUSTED_AUDIENCES, the platform's OAuth client).
// The caller's identity then travels with the request (identity package) and,
// with DownstreamOAuth, so does the token: the Kubernetes API is called as
// the caller.
type OAuthConfig struct {
	// Settings is mcp-oauth's configuration, read from the OAUTH_* variables
	// by oauthconfig.SettingsFromEnv.
	Settings oauthconfig.Settings
	// DownstreamOAuth puts the caller's IdP token on the request so
	// Kubernetes API calls are made as the caller instead of the
	// ServiceAccount (the apiserver must trust the IdP and the token's
	// audience). The ServiceAccount holds no permissions then, so a request
	// that yields no IdP token to present is refused (401).
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
	settings.TrustedIssuers = refuseOpenIDProviderActors(settings.TrustedIssuers)
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

// refuseOpenIDProviderActors gives each trusted issuer marked OpenIDProvider
// an empty actor policy when it has none, so a bearer of that issuer carrying
// an act claim (RFC 8693 §4.1) is refused. Its bearers are ID tokens, which
// identify the person signed in (OpenID Connect Core §2), not an acting party;
// mcp-oauth leaves act unchecked under a nil policy, and OAUTH_TRUSTED_ISSUERS
// cannot set one.
func refuseOpenIDProviderActors(issuers []oauthserver.TrustedIssuer) []oauthserver.TrustedIssuer {
	issuers = slices.Clone(issuers)
	for i := range issuers {
		if issuers[i].OpenIDProvider && issuers[i].Actors == nil {
			issuers[i].Actors = &oauthserver.ActorPolicy{}
		}
	}
	return issuers
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
// server's own access tokens, or a forwarded IdP id_token whose audience is
// trusted) and then attaches the caller to the request.
func (o *oauthRuntime) protect(next http.Handler) http.Handler {
	protected := o.attachIdentity(next)
	if o.downstreamOAuth {
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
		if o.downstreamOAuth {
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
