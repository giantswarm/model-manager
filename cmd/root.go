// Package cmd holds the model-manager CLI.
package cmd

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/giantswarm/mcp-toolkit/logging"
	"github.com/spf13/cobra"

	"github.com/giantswarm/model-manager/internal/buildinfo"
)

// build is the running binary's identity: the release version, the commit
// and the build time, resolved by main from the ldflags values and the Go
// build info.
var build = buildinfo.Info{Version: buildinfo.DevVersion, Commit: buildinfo.UnknownCommit, Date: buildinfo.UnknownDate}

// SetBuild records the build identity the commands report.
func SetBuild(b buildinfo.Info) { build = b }

func newRootCmd() (*cobra.Command, func()) {
	var verbose bool
	var shutdown logging.Shutdown
	root := &cobra.Command{
		Use:   "model-manager",
		Short: "Model management service for the Agent Platform",
		Long: `model-manager exposes one API over per-installation serving backends
(ollama for laptop/agentlab installs, kserve for GPU installs, lemonade for
AMD Ryzen AI hosts running Lemonade Server): list downloaded
and loaded models, pull/import with progress, load/unload, delete, and wire
models into kagent ModelConfigs so agents can use them. The API is served as
REST/JSON (portal) and as MCP tools (muster) from one process.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(command *cobra.Command, _ []string) error {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			options := []logging.Option{
				logging.WithFormat(logging.FormatText), logging.WithLevel(level),
				logging.WithServiceName("model-manager"), logging.WithServiceVersion(build.Version),
			}
			if os.Getenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT") != "" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" || os.Getenv("OTEL_LOGS_EXPORTER") != "" {
				options = append(options, logging.WithStderrMirror())
			}
			logger, flush, err := logging.Init(command.Context(), options...)
			if err != nil {
				return fmt.Errorf("logging: %w", err)
			}
			shutdown = flush
			slog.SetDefault(logger)
			return nil
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = build.Version
	root.SetVersionTemplate("model-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newCacheAgentCmd(), newVersionCmd())
	return root, func() {
		if shutdown != nil {
			flushContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := shutdown(flushContext); err != nil {
				slog.Warn("flushing logs", "error", err)
			}
		}
	}
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "model-manager version %s\n  commit: %s\n  built:  %s\n", build.Version, build.Commit, build.Date)
		},
	}
}

// Execute runs the CLI.
func Execute() {
	root, shutdown := newRootCmd()
	err := root.Execute()
	shutdown()
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
