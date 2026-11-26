// Package requester carries what a request knows about its caller beyond the
// identity mcp-oauth validated (handler.UserInfoFromContext): the caller's own
// OIDC ID token when the server acts on the caller's behalf towards the
// Kubernetes API, and that token's claims. Request continuations retain that
// credential, including after expiry. Application-owned work explicitly
// selects ServiceAccount clients.
package requester

import (
	"context"
	"log/slog"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
)

type ctxKey int

const idTokenKey ctxKey = 0

// FromContext returns the caller mcp-oauth validated, if any.
func FromContext(ctx context.Context) (*providers.UserInfo, bool) {
	info, ok := handler.UserInfoFromContext(ctx)
	return info, ok && info != nil
}

// RequestedBy is the attribution recorded on jobs: the caller's email, else the
// subject, or "" without a caller.
func RequestedBy(ctx context.Context) string {
	info, ok := FromContext(ctx)
	switch {
	case !ok:
		return ""
	case info.Email != "":
		return info.Email
	default:
		return info.ID
	}
}

// Scopes is the validated token's current grant: the scopes mcp-oauth
// validated for this request, else those recorded for the caller.
func Scopes(ctx context.Context) []string {
	if scopes, ok := handler.ScopesFromContext(ctx); ok && len(scopes) > 0 {
		return scopes
	}
	if info, ok := FromContext(ctx); ok {
		return info.Scopes
	}
	return nil
}

// LogAttr is the structured-log attribute every mutation carries: the caller,
// or the ServiceAccount marker when the operation runs without one. It names
// issuer, subject and actor only, never profile data.
func LogAttr(ctx context.Context) slog.Attr {
	if caller, ok := FromContext(ctx); ok {
		if caller.Issuer == "" || caller.ID == "" || (caller.ActorIssuer == "") != (caller.ActorSubject == "") {
			return slog.String("caller", "unavailable")
		}
		attributes := []slog.Attr{slog.String("issuer", caller.Issuer), slog.String("subject", caller.ID)}
		if caller.ActorIssuer != "" {
			attributes = append(attributes, slog.String("actor_issuer", caller.ActorIssuer), slog.String("actor_subject", caller.ActorSubject))
		}
		return slog.GroupAttrs("caller", attributes...)
	}
	return slog.String("caller", "service-account")
}

// ContextWithIDToken returns ctx carrying the caller's OIDC ID token for
// downstream Kubernetes API calls. Only set when the server runs with
// downstream OAuth on.
func ContextWithIDToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, idTokenKey, token)
}

// IDTokenFromContext returns the caller's OIDC ID token, if any.
func IDTokenFromContext(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(idTokenKey).(string)
	return t, ok && t != ""
}

// Claims are the registered claims of a caller's token that explain what
// another party makes of it: who issued it, for whom, until when.
type Claims struct {
	Issuer   string
	Audience []string
	Expiry   time.Time
}

// signatureAlgorithms are the JWS algorithms whose tokens TokenClaims reads.
// The signature is not checked: mcp-oauth verified it when the request came
// in.
var signatureAlgorithms = []jose.SignatureAlgorithm{
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.EdDSA,
}

// TokenClaims reads iss, aud and exp of a JWT without verifying it, for
// messages and expiry checks. The zero Claims when the token is not a JWS.
func TokenClaims(token string) Claims {
	parsed, err := jwt.ParseSigned(token, signatureAlgorithms)
	if err != nil {
		return Claims{}
	}
	var claims jwt.Claims
	if err := parsed.UnsafeClaimsWithoutVerification(&claims); err != nil {
		return Claims{}
	}
	return Claims{Issuer: claims.Issuer, Audience: claims.Audience, Expiry: claims.Expiry.Time()}
}
