package api

import (
	"sync"
	"testing"

	"github.com/giantswarm/mcp-toolkit/tooldesc"
	"github.com/giantswarm/mcp-toolkit/tooldesc/tooldesctest"
	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

// docsServer registers every tool: the set does not depend on the backends
// or the configuration.
var docsServer = sync.OnceValue(func() *mcpserver.MCPServer {
	svc := service.New(nil, jobs.NewManager(), nil, nil, service.Config{}, nil)
	return NewMCPServer(svc, buildinfo.Info{Version: "docs"})
})

// TestToolsDocIsCurrent keeps docs/tools.md — every tool's description and
// input schema as a model reads them — equal to the registered tools.
// TOOLS_DOC_UPDATE=1 rewrites the file (`make tools-doc`).
func TestToolsDocIsCurrent(t *testing.T) {
	tooldesctest.AssertServerDoc(t, docsServer(),
		"# The tools\n\nGenerated from the registered tools (`make tools-doc`); through muster every tool is `x_<server>_<name>`. "+
			"Every configuration registers all of them; `"+ToolAddBackend+"` and `"+ToolRemoveBackend+"` refuse without Kubernetes access.",
		"../../docs/tools.md")
}

// TestToolDescriptionBudgets holds the registered descriptions to the
// budget of giantswarm/giantswarm#38592.
func TestToolDescriptionBudgets(t *testing.T) {
	require.NoError(t, tooldesc.CheckServer(docsServer(), tooldesc.DefaultBudget))
}

// TestReadOnlyToolsAreNotDestructive: a read-only tool says so in full, or
// mcp-go's defaults report it destructive and not idempotent.
func TestReadOnlyToolsAreNotDestructive(t *testing.T) {
	for _, st := range docsServer().ListTools() {
		a := st.Tool.Annotations
		if a.ReadOnlyHint == nil || !*a.ReadOnlyHint {
			continue
		}
		require.Equal(t, mcp.ToBoolPtr(false), a.DestructiveHint, "%s is read-only but reported destructive", st.Tool.Name)
		require.Equal(t, mcp.ToBoolPtr(true), a.IdempotentHint, "%s is read-only but not reported idempotent", st.Tool.Name)
	}
}
