package api

import (
	"testing"

	"github.com/giantswarm/mcp-toolkit/toolpolicy"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

func TestRegisteredToolDuration(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { require.NoError(t, provider.Shutdown(t.Context())) })
	svc := service.New([]backend.Backend{newFakeBackend()}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	server := NewMCPServer(svc, buildinfo.Info{Version: "test"}, WithToolPolicy(toolpolicy.Options{Meter: provider.Meter(TracerName)}))
	_, failed := callTool(t, server, ToolGetInfo, nil)
	require.False(t, failed)
	_, failed = callTool(t, server, ToolGetModel, map[string]any{"model": "missing"})
	require.True(t, failed)
	var resource metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &resource))
	require.Len(t, resource.ScopeMetrics, 1)
	require.Len(t, resource.ScopeMetrics[0].Metrics, 1)
	metric := resource.ScopeMetrics[0].Metrics[0]
	require.Equal(t, "mcp.server.operation.duration", metric.Name)
	require.Equal(t, "s", metric.Unit)
	histogram, ok := metric.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	outcomes := map[string]string{}
	for _, point := range histogram.DataPoints {
		require.Equal(t, uint64(1), point.Count)
		method, _ := point.Attributes.Value("mcp.method.name")
		require.Equal(t, "tools/call", method.AsString())
		operation, _ := point.Attributes.Value("gen_ai.operation.name")
		require.Equal(t, "execute_tool", operation.AsString())
		tool, _ := point.Attributes.Value("gen_ai.tool.name")
		failure, _ := point.Attributes.Value("error.type")
		outcomes[tool.AsString()] = failure.AsString()
	}
	require.Equal(t, map[string]string{ToolGetInfo: "", ToolGetModel: "tool_error"}, outcomes)
}

func TestRegisteredToolWholeResultLimit(t *testing.T) {
	svc := service.New(nil, jobs.NewManager(), nil, nil, service.Config{}, nil)
	for _, limit := range []int{32, 0, -1} {
		server := NewMCPServer(svc, buildinfo.Info{Version: "test"}, WithToolPolicy(toolpolicy.Options{MaxResultBytes: limit}))
		_, failed := callTool(t, server, ToolGetInfo, nil)
		require.Equal(t, limit > 0, failed)
	}
}
