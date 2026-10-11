# Interface dependencies

The interface uses these direct dependencies. The pinned versions support
Go 1.22:

- `charmbracelet/huh` v0.6.0, `bubbletea` v1.1.0, `bubbles` v0.20.0, and
  `lipgloss` v0.13.0 for forms, the dashboard, and text layout.
- `mattn/go-isatty` for terminal detection.
- `mattn/go-runewidth` for display-width text wrapping.

`internal/ui` never imports `internal/looper`. The interface does not need
Markdown rendering, so it does not use Glamour.
