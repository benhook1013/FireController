package mcpadapter_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/profile"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const mcpInventoryDoc = "../../docs/mcp.md"

func documentedMCPTools(document string) ([]string, error) {
	section, table := false, false
	var names []string
	for _, line := range strings.Split(document, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "## ") {
			if section {
				break
			}
			section = line == "## Tools and output"
		}
		if !section {
			continue
		}
		if !table {
			table = line == "| Tool | Input and result |"
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if len(names) > 0 {
				break
			}
			continue
		}
		cells := strings.SplitN(line, "|", 3)
		if len(cells) != 3 {
			return nil, fmt.Errorf("malformed inventory row")
		}
		cell := strings.TrimSpace(cells[1])
		if strings.Trim(cell, "-: ") == "" {
			continue
		}
		if len(cell) < 3 || cell[0] != '`' || cell[len(cell)-1] != '`' || strings.ContainsAny(cell[1:len(cell)-1], "` \t") {
			return nil, fmt.Errorf("malformed tool name in inventory")
		}
		names = append(names, cell[1:len(cell)-1])
	}
	if !table || len(names) == 0 {
		return nil, fmt.Errorf("tool inventory table is missing or empty")
	}
	return names, nil
}

func compareMCPInventory(document string, discovered []string) error {
	documented, err := documentedMCPTools(document)
	if err != nil {
		return fmt.Errorf("%s: %w", mcpInventoryDoc, err)
	}
	docs, actual := map[string]int{}, map[string]int{}
	for _, name := range documented {
		docs[name]++
	}
	for _, name := range discovered {
		actual[name]++
	}
	var missing, extra, duplicates, discoveryDuplicates []string
	for name, n := range docs {
		if n > 1 {
			duplicates = append(duplicates, name)
		}
		if actual[name] == 0 {
			extra = append(extra, name)
		}
	}
	for name, n := range actual {
		if n > 1 {
			discoveryDuplicates = append(discoveryDuplicates, name)
		}
		if docs[name] == 0 {
			missing = append(missing, name)
		}
	}
	if len(missing)+len(extra)+len(duplicates)+len(discoveryDuplicates) == 0 {
		return nil
	}
	for _, names := range [][]string{missing, extra, duplicates, discoveryDuplicates} {
		slices.Sort(names)
	}
	return fmt.Errorf("%s: missing from documentation=%v; not in SDK discovery=%v; duplicate documented tools=%v; duplicate discovered tools=%v", mcpInventoryDoc, missing, extra, duplicates, discoveryDuplicates)
}

func TestMCPDocumentInventoryMatchesSDKDiscovery(t *testing.T) {
	document, err := os.ReadFile(mcpInventoryDoc)
	if err != nil {
		t.Fatal(err)
	}
	binding := localContext()
	session := connect(t, adapter(t, profiles(t, map[string]profile.Context{"chosen": binding}, "missing-inventory-credential"), "chosen"), "2025-11-25")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var discovered []string
	params := &mcp.ListToolsParams{}
	seen := map[string]bool{}
	for {
		listed, err := session.ListTools(ctx, params)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range listed.Tools {
			discovered = append(discovered, tool.Name)
		}
		if listed.NextCursor == "" {
			break
		}
		if seen[listed.NextCursor] {
			t.Fatal("SDK tool discovery repeated a page cursor")
		}
		seen[listed.NextCursor] = true
		params.Cursor = listed.NextCursor
	}
	if err = compareMCPInventory(string(document), discovered); err != nil {
		t.Fatal(err)
	}
	names, err := documentedMCPTools(string(document))
	if err != nil {
		t.Fatal(err)
	}
	var row string
	for _, line := range strings.Split(string(document), "\n") {
		if strings.HasPrefix(line, "| `"+names[0]+"` |") {
			row = line
			break
		}
	}
	if row == "" {
		t.Fatal("inventory fixture row unavailable")
	}
	// Exercise the real comparison against edited in-memory documents. The
	// tracked owner and its hand-authored descriptions are never changed.
	for _, test := range []struct{ name, document, want string }{
		{"missing", strings.Replace(string(document), row+"\n", "", 1), names[0]},
		{"duplicate", strings.Replace(string(document), row, row+"\n"+row, 1), names[0]},
		{"extra", strings.Replace(string(document), row, row+"\n| `documentation_only_fixture_tool` | Fixture only. |", 1), "documentation_only_fixture_tool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := compareMCPInventory(test.document, discovered)
			if err == nil || !strings.Contains(err.Error(), mcpInventoryDoc) || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("inventory drift was not diagnosed: %v", err)
			}
			t.Logf("in-memory drift rejected: %v", err)
		})
	}
}
