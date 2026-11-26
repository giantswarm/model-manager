package cmd

import (
	"context"
	"log/slog"

	"github.com/giantswarm/mcp-oauth/handler"
)

// validatedUser is the user mcp-oauth validated for the tool audit record, or
// nil. providers.UserInfo logs issuer, subject and actor, never profile data.
func validatedUser(ctx context.Context) slog.LogValuer {
	user, ok := handler.UserInfoFromContext(ctx)
	if !ok || user == nil {
		return nil
	}
	return user
}
