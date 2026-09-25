package api

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	mcpotel "github.com/mark3labs/mcp-go/otel"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.41.0"
	"go.opentelemetry.io/otel/trace"
)

// TracerName is the instrumentation scope of model-manager's own spans.
const TracerName = "github.com/giantswarm/model-manager"

// tracingOptions emit an mcp.<method> server span for every JSON-RPC
// request and a tool.<name> span around each tool handler. The tools/call
// server span also carries gen_ai.tool.name, the attribute muster puts on its
// side of the call. The propagator extracts nothing: the HTTP server span in
// the request context already joined the caller's traceparent, and the MCP
// span nests under it. Without a configured exporter the global provider is
// a no-op.
func tracingOptions() []mcpserver.ServerOption {
	return []mcpserver.ServerOption{
		mcpserver.WithToolHandlerMiddleware(toolNameAttribute),
		mcpotel.WithServerTracingPropagator(otel.Tracer(TracerName), propagation.NewCompositeTextMapPropagator()),
	}
}

// toolNameAttribute runs outside mcp-go's tool span, so the span in ctx is
// the tools/call server span.
func toolNameAttribute(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		trace.SpanFromContext(ctx).SetAttributes(semconv.GenAIToolName(req.Params.Name))
		return next(ctx, req)
	}
}
