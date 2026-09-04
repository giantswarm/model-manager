// Package cmd holds the model-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	version     = "dev"
	buildCommit = "unknown"
	buildDate   = "unknown"
)

// SetVersion records the build version (set from main via ldflags).
func SetVersion(v string) { version = v }

// SetBuildInfo records the commit and build date.
func SetBuildInfo(commit, date string) {
	buildCommit = commit
	buildDate = date
}

func newRootCmd() *cobra.Command {
	var verbose bool
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
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			level := slog.LevelInfo
			if verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, "Enable debug logging")
	root.Version = version
	root.SetVersionTemplate("model-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newCacheAgentCmd(), newVersionCmd())
	return root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Run: func(cmd *cobra.Command, _ []string) {
			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "model-manager version %s\n  commit: %s\n  built:  %s\n", version, buildCommit, buildDate)
		},
	}
}

// Execute runs the CLI.
func Execute() {
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
