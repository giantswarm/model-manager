package api

import (
	"os"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestChartApprovalMapNamesTheRegisteredTools keeps the chart's tool approval
// map (muster.mcpServer.approval.tools in helm/model-manager/values.yaml) equal to
// the tools the server registers: the chart writes every tool out explicitly,
// so a tool missing from it silently falls to the rule and a stale key
// misleads the reviewer.
func TestChartApprovalMapNamesTheRegisteredTools(t *testing.T) {
	raw, err := os.ReadFile("../../helm/model-manager/values.yaml")
	if err != nil {
		t.Fatalf("read the chart values: %v", err)
	}
	var values struct {
		Muster struct {
			MCPServer struct {
				Approval struct {
					Tools map[string]string `json:"tools"`
				} `json:"approval"`
			} `json:"mcpServer"`
		} `json:"muster"`
	}
	if err := yaml.Unmarshal(raw, &values); err != nil {
		t.Fatalf("parse the chart values: %v", err)
	}

	var inChart []string
	for name := range values.Muster.MCPServer.Approval.Tools {
		inChart = append(inChart, name)
	}
	registered := append([]string(nil), ToolNames()...)
	sort.Strings(inChart)
	sort.Strings(registered)

	if strings.Join(inChart, ",") != strings.Join(registered, ",") {
		t.Fatalf("helm/model-manager/values.yaml muster.mcpServer.approval.tools must name exactly the registered tools.\n"+
			"chart:      %v\nregistered: %v\n"+
			"Add a new tool to (or remove a deleted one from) approval.tools in helm/model-manager/values.yaml and in helm/model-manager/values.schema.json, and to the README.md default.",
			inChart, registered)
	}
}
