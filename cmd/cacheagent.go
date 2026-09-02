package cmd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/giantswarm/model-manager/internal/cacheagent"
)

type cacheAgentOptions struct {
	listen string
	root   string
	node   string
}

// newCacheAgentCmd is the DaemonSet side of kserve.inventory.mode=daemonset:
// it mounts the cache claim read-only and serves its contents to
// model-manager, which otherwise runs a scan pod per node.
func newCacheAgentCmd() *cobra.Command {
	o := &cacheAgentOptions{}
	cmd := &cobra.Command{
		Use:   "cache-agent",
		Short: "Serve the contents of a mounted model cache over HTTP (DaemonSet inventory)",
		Long: `Run the cache agent: walk the mounted model cache (one directory per
InferenceService plus the pre-warm download markers) on every request and
serve the result as JSON at ` + cacheagent.InventoryPath + `. The kserve driver of
model-manager reads it from the agent pod on a node instead of creating a scan
pod there (kserve.inventory.mode=daemonset in the chart).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCacheAgent(cmd.Context(), o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.listen, "listen", envOr("CACHE_AGENT_LISTEN", fmt.Sprintf(":%d", cacheagent.DefaultPort)), "Listen address (CACHE_AGENT_LISTEN)")
	f.StringVar(&o.root, "cache-root", envOr("CACHE_AGENT_ROOT", cacheagent.DefaultRoot), "Where the cache claim is mounted (CACHE_AGENT_ROOT)")
	f.StringVar(&o.node, "node-name", envOr("NODE_NAME", ""), "Node this agent runs on, reported with the inventory (NODE_NAME, from the downward API)")
	return cmd
}

func runCacheAgent(ctx context.Context, o *cacheAgentOptions) error {
	log := slog.Default().With("component", "cache-agent")
	if _, err := os.Stat(o.root); err != nil {
		return fmt.Errorf("cache root %s: %w", o.root, err)
	}
	srv := &http.Server{
		Addr:              o.listen,
		Handler:           cacheagent.Handler(o.root, o.node, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		log.Info("cache agent listening", "version", version, "listen", o.listen, "root", o.root, "node", o.node)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
