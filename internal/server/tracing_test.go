package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/giantswarm/model-manager/internal/api"
	"github.com/giantswarm/model-manager/internal/backend"
	"github.com/giantswarm/model-manager/internal/buildinfo"
	"github.com/giantswarm/model-manager/internal/jobs"
	"github.com/giantswarm/model-manager/internal/service"
)

const (
	callerTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	callerSpanID  = "00f067aa0ba902b7"
)

// recordSpans installs a recording tracer provider and the W3C propagator
// globally for the test, as tracing.Init does in the binary.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
	})
	return rec
}

func mcpPost(t *testing.T, url, session, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("traceparent", "00-"+callerTraceID+"-"+callerSpanID+"-01")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	return resp
}

func spanNamed(t *testing.T, spans []sdktrace.ReadOnlySpan, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.Name() == name {
			return s
		}
	}
	names := make([]string, 0, len(spans))
	for _, s := range spans {
		names = append(names, s.Name())
	}
	require.Failf(t, "span not recorded", "want %q, got %v", name, names)
	return nil
}

func spanByID(t *testing.T, spans []sdktrace.ReadOnlySpan, id trace.SpanID) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range spans {
		if s.SpanContext().SpanID() == id {
			return s
		}
	}
	require.Failf(t, "span not recorded", "want span %s", id)
	return nil
}

func TestToolCallIsTracedUnderTheCallersTrace(t *testing.T) {
	rec := recordSpans(t)
	svc := service.New([]backend.Backend{downBackend{}}, jobs.NewManager(), nil, nil, service.Config{}, nil)
	srv, err := New(Config{Addr: "127.0.0.1:0", MCPEnabled: true}, svc, api.NewMCPServer(svc, buildinfo.Info{Version: "test"}), nil)
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := mcpPost(t, ts.URL, "", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	session := resp.Header.Get("Mcp-Session-Id")
	mcpPost(t, ts.URL, session, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_info","arguments":{}}}`)

	probe, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	_ = probe.Body.Close()

	spans := rec.Ended()
	for _, s := range spans {
		require.Equal(t, callerTraceID, s.SpanContext().TraceID().String(), "span %q joins the caller's trace", s.Name())
		require.NotEqual(t, "GET /healthz", s.Name(), "the probes are not traced")
	}

	call := spanNamed(t, spans, "mcp.tools/call")
	require.Equal(t, trace.SpanKindServer, call.SpanKind())
	httpSpan := spanByID(t, spans, call.Parent().SpanID())
	require.Equal(t, "POST /mcp", httpSpan.Name(), "the MCP span nests under the HTTP span")
	require.Equal(t, trace.SpanKindServer, httpSpan.SpanKind())
	require.Equal(t, callerSpanID, httpSpan.Parent().SpanID().String())
	require.Contains(t, call.Attributes(), attribute.String("gen_ai.tool.name", "get_info"))
	require.Contains(t, call.Attributes(), attribute.String("mcp.tool.name", "get_info"))

	tool := spanNamed(t, spans, "tool.get_info")
	require.Equal(t, call.SpanContext().SpanID(), tool.Parent().SpanID())
}
