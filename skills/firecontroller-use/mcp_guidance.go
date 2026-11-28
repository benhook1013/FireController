// Package useguidance exposes the MCP instructions from the bundled usage skill.
package useguidance

import (
	_ "embed"
	"strings"
)

//go:embed SKILL.md
var skill string

const (
	startMarker = "<!-- BEGIN MCP SERVER INSTRUCTIONS -->"
	endMarker   = "<!-- END MCP SERVER INSTRUCTIONS -->"
)

// MCPInstructions is the transport-neutral guidance derived from the usage skill.
func MCPInstructions() string {
	_, section, _ := strings.Cut(skill, startMarker)
	instructions, _, _ := strings.Cut(section, endMarker)
	if strings.Count(skill, startMarker) != 1 || strings.Count(skill, endMarker) != 1 ||
		!strings.Contains(section, endMarker) || strings.TrimSpace(instructions) == "" {
		panic("firecontroller-use skill has invalid MCP instruction markers")
	}
	return strings.TrimSpace(instructions)
}
