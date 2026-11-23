package cmd

import (
	"context"

	"github.com/giantswarm/mcp-oauth/handler"
	"github.com/giantswarm/mcp-toolkit/toolpolicy"
)

func validatedToolIdentity(ctx context.Context) (toolpolicy.Identity, bool) {
	user, ok := handler.UserInfoFromContext(ctx)
	if !ok || user == nil {
		return toolpolicy.Identity{}, false
	}
	return toolpolicy.Identity{Issuer: user.Issuer, Subject: user.ID, ActorIssuer: user.ActorIssuer, ActorSubject: user.ActorSubject}, true
}
