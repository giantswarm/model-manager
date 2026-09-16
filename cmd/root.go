// Package cmd holds the model-manager CLI.
package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/giantswarm/model-manager/internal/buildinfo"
)

// build is the running binary's identity: the release version, the commit
// and the build time, resolved by main from the ldflags values and the Go
// build info.
var build = buildinfo.Info{Version: buildinfo.DevVersion, Commit: buildinfo.UnknownCommit, Date: buildinfo.UnknownDate}

// SetBuild records the build identity the commands report.
func SetBuild(b buildinfo.Info) { build = b }

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
	root.Version = build.Version
	root.SetVersionTemplate("model-manager version {{.Version}}\n")
	root.AddCommand(newServeCmd(), newCacheAgentCmd(), newVersionCmd())
	return root
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
	if err := newRootCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
