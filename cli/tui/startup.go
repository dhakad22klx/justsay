package tui

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/chzyer/readline"
)

// IntegrationStatus is safe display metadata, never an account or token.
type IntegrationStatus struct {
	Name, Status, Detail string
	Ready                bool
}

// Startup writes the entire panel together so background messages cannot split
// its borders. Like the welcome banner, it is not part of the conversation log.
func (o *Output) Startup(model string, systems []IntegrationStatus) {
	width, _, _ := readline.GetSize(int(os.Stdout.Fd()))
	_, noColor := os.LookupEnv("NO_COLOR")
	color := readline.IsTerminal(int(os.Stdout.Fd())) && !noColor && os.Getenv("TERM") != "dumb"
	o.mu.Lock()
	defer o.mu.Unlock()
	fmt.Fprint(o.stdout, startupPanel(model, systems, width, color))
}

func startupPanel(model string, systems []IntegrationStatus, width int, color bool) string {
	paint := func(s string, style func(string) string) string {
		if color {
			return style(s)
		}
		return s
	}
	// A compact, unboxed layout fits split panes. Unknown widths use the
	// normal panel, including output redirected to a file.
	boxed := width == 0 || width >= 48
	const inside = 44
	var b strings.Builder
	b.WriteByte('\n')
	line := func(s string, style func(string) string) {
		if boxed {
			padding := strings.Repeat(" ", max(0, inside-2-utf8.RuneCountInString(s)))
			b.WriteString(paint("│", Gray))
			b.WriteString("  ")
			b.WriteString(paint(s, style))
			b.WriteString(padding)
			b.WriteString(paint("│", Gray))
		} else {
			b.WriteString(paint(s, style))
		}
		b.WriteByte('\n')
	}
	border := func(left, right string) {
		if boxed {
			b.WriteString(paint(left+strings.Repeat("─", inside)+right, Gray))
			b.WriteByte('\n')
		}
	}
	border("╭", "╮")
	line("Justsay · Your personal AI assistant", Blue)
	// Model IDs come from configuration; keep controls and long values out
	// of the frame without changing the provider's actual model setting.
	model = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, model)
	if runes := []rune(model); len(runes) > inside-12 {
		model = string(runes[:inside-15]) + "..."
	}
	line("Model  ·  "+model, Gray)
	border("├", "┤")
	for _, system := range systems {
		marker, style := "○", Yellow
		if system.Ready {
			marker, style = "●", Green
		}
		line(fmt.Sprintf("%s  %-10s %s", marker, system.Name, system.Status), style)
	}
	border("├", "┤")
	line("Saved status · help / reset / exit", Gray)
	border("╰", "╯")
	b.WriteByte('\n')
	return b.String()
}
