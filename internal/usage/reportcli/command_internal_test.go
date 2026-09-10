package reportcli

import (
	"io"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestSurfaceTitleStylesUseDistinctBrandBackgrounds(t *testing.T) {
	renderer := lipgloss.NewRenderer(io.Discard)
	for _, tc := range []struct {
		name       string
		background lipgloss.Color
	}{
		{"Anthropic", "#D97757"},
		{"OpenAI", "#10A37F"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			style := surfaceTitleStyle(renderer, tc.background)
			if !style.GetBold() {
				t.Error("Surface title is not bold")
			}
			if got := style.GetForeground(); got != lipgloss.Color("#000000") {
				t.Errorf("foreground = %v, want black", got)
			}
			if got := style.GetBackground(); got != tc.background {
				t.Errorf("background = %v, want %v", got, tc.background)
			}
		})
	}
	if anthropicSurfaceColor == openAISurfaceColor {
		t.Fatal("Surface backgrounds must remain distinct")
	}
}
