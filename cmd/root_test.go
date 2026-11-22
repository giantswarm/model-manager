package cmd

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRootLoggingConfiguration(t *testing.T) {
	for _, mode := range []string{"info", "verbose", "otlp-none", "invalid-exporter"} {
		t.Run(mode, func(t *testing.T) {
			previous := slog.Default()
			t.Cleanup(func() { slog.SetDefault(previous) })
			for _, name := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "OTEL_LOGS_EXPORTER"} {
				t.Setenv(name, "")
			}
			if mode == "otlp-none" {
				t.Setenv("OTEL_LOGS_EXPORTER", "none")
			}
			if mode == "invalid-exporter" {
				t.Setenv("OTEL_LOGS_EXPORTER", "unknown-exporter")
			}
			command, flush := newRootCmd()
			defer flush()
			command.SetOut(io.Discard)
			arguments := []string{"version"}
			if mode == "verbose" {
				arguments = append(arguments, "--verbose")
			}
			command.SetArgs(arguments)
			err := command.ExecuteContext(t.Context())
			if mode == "invalid-exporter" {
				require.ErrorContains(t, err, "logging:")
				return
			}
			require.NoError(t, err)
			if mode != "otlp-none" {
				require.Equal(t, mode == "verbose", slog.Default().Enabled(t.Context(), slog.LevelDebug))
			}
		})
	}
}
