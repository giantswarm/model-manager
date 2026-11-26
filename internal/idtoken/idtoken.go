// Package idtoken carries the user's OpenID Connect ID token on a request
// context. It is set only when the server calls the Kubernetes API as the
// user (--downstream-oauth): mcp-oauth's handler.ProviderIDTokenFromContext
// returns a forwarded ID token in either mode, so it cannot tell the two
// apart. Request continuations retain the token, including after expiry;
// application-owned work carries none and selects ServiceAccount clients.
package idtoken

import "context"

type contextKey struct{}

// ContextWith returns ctx carrying token. An empty token adds nothing.
func ContextWith(ctx context.Context, token string) context.Context {
	if token == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, token)
}

// FromContext returns the user's ID token, if the request carries one.
func FromContext(ctx context.Context) (string, bool) {
	token, ok := ctx.Value(contextKey{}).(string)
	return token, ok && token != ""
}
