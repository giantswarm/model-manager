package service

import (
	"context"
	"log/slog"

	"github.com/giantswarm/mcp-oauth/handler"
)

// userAttr names the user a call acts for by issuer, subject and actor
// (providers.UserInfo's LogValue, never profile data). Work without a user,
// such as the wiring reconciler, logs no user.
func userAttr(ctx context.Context) slog.Attr {
	user, _ := handler.UserInfoFromContext(ctx)
	return slog.Any("user", user)
}
