package backend

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// Options carries per-driver configuration. Each driver reads its own block;
// the kserve driver adds a KServe block when it lands.
type Options struct {
	Ollama OllamaOptions
}

// OllamaOptions configures the ollama driver.
type OllamaOptions struct {
	// Endpoint is the Ollama API base URL as reached by model-manager.
	Endpoint string
	// AgentHost is the Ollama host written into kagent ModelConfigs (as
	// reached by agent pods). Defaults to Endpoint.
	AgentHost string
	// Timeout bounds non-streaming API calls.
	Timeout time.Duration
}

// Factory builds a Backend from Options.
type Factory func(opts Options) (Backend, error)

var (
	registryMu sync.RWMutex
	registry   = map[Name]Factory{}
)

// Register makes a driver available to New. Drivers are registered
// explicitly by the command wiring, not via init side effects.
func Register(name Name, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// New builds the named driver.
func New(name Name, opts Options) (Backend, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (available: %s)", name, strings.Join(Names(), ", "))
	}
	return f(opts)
}

// Names lists the registered drivers, sorted.
func Names() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, string(n))
	}
	sort.Strings(names)
	return names
}
