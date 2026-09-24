package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/openapi"
)

// schemaTTL is how long an answer about the served ModelConfig schema holds:
// a kagent upgrade that adds a field reaches the ModelConfigs within it,
// without a restart of model-manager.
const schemaTTL = time.Minute

// servedSchema answers which fields the ModelConfig schema the apiserver
// serves has, from the OpenAPI v3 document it publishes for the kagent.dev
// group version. The apiserver prunes a field its CRD lacks from every write,
// so such a field is left out of the write, and out of the comparison that
// decides a re-wire, which would otherwise re-wire on every pass. The
// document is readable by every authenticated identity (system:discovery),
// so the ServiceAccount reads it in every mode.
type servedSchema struct {
	client  openapi.ClientWithContext
	version string
	now     func() time.Time

	mu      sync.Mutex
	ollama  map[string]bool
	fetched time.Time
}

// schemaNode is the part of an OpenAPI v3 schema the lookup walks.
type schemaNode struct {
	Properties map[string]schemaNode `json:"properties"`
	GVK        []struct {
		Group   string `json:"group"`
		Version string `json:"version"`
		Kind    string `json:"kind"`
	} `json:"x-kubernetes-group-version-kind"`
}

// ollamaFields returns the property names of spec.ollama in the served
// ModelConfig schema.
func (s *servedSchema) ollamaFields(ctx context.Context) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ollama != nil && s.now().Sub(s.fetched) < schemaTTL {
		return s.ollama, nil
	}
	fields, err := s.fetchOllamaFields(ctx)
	if err != nil {
		return nil, err
	}
	s.ollama, s.fetched = fields, s.now()
	return fields, nil
}

func (s *servedSchema) fetchOllamaFields(ctx context.Context) (map[string]bool, error) {
	gv := KagentGroup + "/" + s.version
	paths, err := s.client.PathsWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the apiserver's OpenAPI v3 paths: %w", err)
	}
	p, ok := paths["apis/"+gv]
	if !ok {
		return nil, fmt.Errorf("the apiserver publishes no OpenAPI v3 schema for %s (is kagent installed?)", gv)
	}
	raw, err := p.SchemaWithContext(ctx, runtime.ContentTypeJSON)
	if err != nil {
		return nil, fmt.Errorf("read the OpenAPI v3 schema of %s: %w", gv, err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]schemaNode `json:"schemas"`
		} `json:"components"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("decode the OpenAPI v3 schema of %s: %w", gv, err)
	}
	for _, sch := range doc.Components.Schemas {
		for _, k := range sch.GVK {
			if k.Group != KagentGroup || k.Version != s.version || k.Kind != "ModelConfig" {
				continue
			}
			fields := map[string]bool{}
			for name := range sch.Properties["spec"].Properties["ollama"].Properties {
				fields[name] = true
			}
			return fields, nil
		}
	}
	return nil, fmt.Errorf("the OpenAPI v3 schema of %s has no ModelConfig", gv)
}
