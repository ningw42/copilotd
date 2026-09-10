package reportcli

import (
	"io"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestSurfaceTitleStylesUseTerminalBaseAndDistinctBrandBackgrounds(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	terminalBase := lipgloss.Color("#1E1E2E")
	for _, tc := range []struct {
		name       string
		background lipgloss.Color
	}{
		{"Anthropic", "#D97757"},
		{"OpenAI", "#10A37F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			style := surfaceTitleStyle(renderer, terminalBase, tc.background)
			if !style.GetBold() {
				t.Error("Surface title is not bold")
			}
			if got := style.GetForeground(); got != terminalBase {
				t.Errorf("foreground = %v, want terminal base %v", got, terminalBase)
			}
			if got := style.GetBackground(); got != tc.background {
				t.Errorf("background = %v, want %v", got, tc.background)
			}
			if top, right, bottom, left := style.GetPadding(); top != 0 || right != 1 || bottom != 0 || left != 1 {
				t.Errorf("padding = (%d, %d, %d, %d), want (0, 1, 0, 1)", top, right, bottom, left)
			}
		})
	}
	if anthropicSurfaceColor == openAISurfaceColor {
		t.Fatal("Surface backgrounds must remain distinct")
	}
}

func TestTerminalBackgroundColorFallsBackToDefaultOutsideTTY(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	if got := terminalBackgroundColor(renderer); got != (lipgloss.NoColor{}) {
		t.Errorf("non-TTY background = %v, want default color", got)
	}
}
