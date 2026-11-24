// Package requester carries what a request knows about its caller beyond the
// identity mcp-oauth validated (handler.UserInfoFromContext): the caller's own
// IdP token when the server acts on the caller's behalf towards the
// Kubernetes API, that token's claims, and the person's GitHub grant for a
// commit. Request continuations retain that credential, including after
// expiry. Application-owned work explicitly selects ServiceAccount clients.
package requester

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"strings"
	"time"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-oauth/providers"
)

type ctxKey int

const tokenKey ctxKey = 0

// FromContext returns the caller mcp-oauth validated, if any.
func FromContext(ctx context.Context) (*providers.UserInfo, bool) {
	info, ok := handler.UserInfoFromContext(ctx)
	return info, ok && info != nil
}

// Caller is the attribution recorded on jobs: the caller's email, else the
// subject, or "" without a caller.
func Caller(ctx context.Context) string {
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

// ContextWithToken returns ctx carrying the caller's IdP token for downstream
// Kubernetes API calls. Only set when the server runs with downstream OAuth on.
func ContextWithToken(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, tokenKey, token)
}

// TokenFromContext returns the caller's IdP token, if any.
func TokenFromContext(ctx context.Context) (string, bool) {
	t, ok := ctx.Value(tokenKey).(string)
	return t, ok && t != ""
}

// TokenExpiry reads the `exp` claim of a JWT without verifying it — the token
// was validated when the request came in; this only decides whether a
// background continuation may still present it. Zero when the token is not a
// JWT or carries no exp.
func TokenExpiry(token string) time.Time { return TokenClaims(token).Expires }

// Claims are the registered claims of a caller's token that explain what
// another party makes of it: who issued it, for whom, until when.
type Claims struct {
	Issuer   string
	Audience []string
	Expires  time.Time
}

// TokenClaims reads iss, aud and exp of a JWT without verifying it (the token
// was validated when the request came in), for messages and expiry checks.
// The zero Claims when the token is not a JWT.
func TokenClaims(token string) Claims {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Claims{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}
	}
	var raw struct {
		Iss string          `json:"iss"`
		Aud json.RawMessage `json:"aud"`
		Exp json.Number     `json:"exp"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return Claims{}
	}
	c := Claims{Issuer: raw.Iss}
	// aud is a string or an array of strings (RFC 7519 4.1.3).
	var one string
	if json.Unmarshal(raw.Aud, &one) == nil && one != "" {
		c.Audience = []string{one}
	} else {
		_ = json.Unmarshal(raw.Aud, &c.Audience)
	}
	if exp, err := raw.Exp.Int64(); err == nil && exp > 0 {
		c.Expires = time.Unix(exp, 0)
	}
	return c
}

// GitHub is the caller's GitHub identity for one commit: the person's user
// token of the App giantswarm-model-manager, fetched from muster's broker,
// and the login GET /user answered for it. Commit mode opens the pull
// request with that token.
type GitHub struct {
	Login string
	Token string
}
