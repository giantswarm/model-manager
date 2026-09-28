package api

import (
	"context"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/semconv/v1.41.0/mcpconv"
)

// errorTypeTool is error.type on mcp.server.operation.duration for a tool
// result flagged isError; a handler that returned a Go error records
// mcpconv.ErrorTypeOther.
const errorTypeTool mcpconv.ErrorTypeAttr = "tool_error"

// toolDuration records mcp.server.operation.duration for every tools/call,
// labelled with gen_ai.tool.name and, on failure, error.type. The tool set is
// fixed at registration, so the tool name is a bounded label. Without a
// configured MeterProvider the global one is a no-op.
func toolDuration(meter metric.Meter) mcpserver.ToolHandlerMiddleware {
	hist, err := mcpconv.NewServerOperationDuration(meter)
	if err != nil {
		otel.Handle(err)
	}
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			start := time.Now()
			res, err := next(ctx, req)
			attrs := []attribute.KeyValue{
				hist.AttrGenAIToolName(req.Params.Name),
				hist.AttrGenAIOperationName(mcpconv.GenAIOperationNameExecuteTool),
			}
			switch {
			case err != nil:
				attrs = append(attrs, hist.AttrErrorType(mcpconv.ErrorTypeOther))
			case res != nil && res.IsError:
				attrs = append(attrs, hist.AttrErrorType(errorTypeTool))
			}
			hist.Record(ctx, time.Since(start).Seconds(), mcpconv.MethodNameToolsCall, attrs...)
			return res, err
		}
	}
}
