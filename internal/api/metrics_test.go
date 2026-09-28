package api

import (
	"context"
	"errors"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestToolDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	mw := toolDuration(provider.Meter(TracerName))

	handlers := map[string]func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error){
		ToolListModels: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultText("[]"), nil
		},
		ToolLoadModel: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return mcp.NewToolResultError("no such model"), nil
		},
		ToolPullModel: func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return nil, errors.New("backend down")
		},
	}
	for name, h := range handlers {
		var req mcp.CallToolRequest
		req.Params.Name = name
		_, _ = mw(h)(t.Context(), req)
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))
	require.Len(t, rm.ScopeMetrics, 1)
	require.Len(t, rm.ScopeMetrics[0].Metrics, 1)
	m := rm.ScopeMetrics[0].Metrics[0]
	require.Equal(t, "mcp.server.operation.duration", m.Name)
	require.Equal(t, "s", m.Unit)
	hist, ok := m.Data.(metricdata.Histogram[float64])
	require.True(t, ok)

	got := map[string]string{}
	for _, dp := range hist.DataPoints {
		require.Equal(t, uint64(1), dp.Count)
		method, _ := dp.Attributes.Value("mcp.method.name")
		require.Equal(t, "tools/call", method.AsString())
		op, _ := dp.Attributes.Value("gen_ai.operation.name")
		require.Equal(t, "execute_tool", op.AsString())
		tool, _ := dp.Attributes.Value("gen_ai.tool.name")
		errType, _ := dp.Attributes.Value(attribute.Key("error.type"))
		got[tool.AsString()] = errType.AsString()
	}
	require.Equal(t, map[string]string{
		ToolListModels: "",
		ToolLoadModel:  "tool_error",
		ToolPullModel:  "_OTHER",
	}, got)
}
