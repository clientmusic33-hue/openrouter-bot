package agent

import (
	"fmt"
	"strings"

	"openrouter-bot/tools"
)

// FormatToolCatalog builds the system-prompt description of all tools permitted
// for the user's role.
func FormatToolCatalog(reg *tools.Registry, role string) string {
	if reg == nil {
		return "No external tools available."
	}

	available := reg.ListForRole(role)
	if len(available) == 0 {
		return "No external tools available."
	}

	var sb strings.Builder
	sb.WriteString("Available tools:\n")
	for _, t := range available {
		sb.WriteString(fmt.Sprintf("- %s: %s (input: %s)\n", t.Name, t.Description, t.InputSchema))
	}
	return strings.TrimSpace(sb.String())
}
