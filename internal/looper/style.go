package looper

import (
	"io"
	"os"

	"github.com/mattn/go-isatty"
)

// OutputMode selects the rendering path for human output. Machine output
// through --json is separate from both modes.
type OutputMode int

const (
	// ModePlain carries no ANSI escapes, animation, forms, or glyphs.
	ModePlain OutputMode = iota
	// ModeInteractive renders labels, color, glyphs, and timestamps.
	ModeInteractive
)

// isTTY reports whether a file is an interactive terminal.
func isTTY(f *os.File) bool {
	if f == nil {
		return false
	}
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// writerTTY reports whether an arbitrary writer is a terminal. Writers that
// are not files (tests, pipes, captures) are never terminals.
func writerTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	return ok && isTTY(f)
}

// terminalInteractive reports whether the terminal type supports full-screen
// or cursor-addressed interaction. TERM=dumb gets plain output.
func terminalInteractive() bool {
	term := os.Getenv("TERM")
	return term != "" && term != "dumb"
}

// DetectMode picks the output mode for a stream pair. Explicit --plain wins
// over a terminal; --json never reaches this function's callers for styling,
// but suppresses the interactive mode too.
func DetectMode(in *os.File, out io.Writer, plain bool) OutputMode {
	if plain {
		return ModePlain
	}
	if !isTTY(in) || !writerTTY(out) {
		return ModePlain
	}
	if !terminalInteractive() {
		return ModePlain
	}
	if os.Getenv("NO_COLOR") != "" {
		// Color is disabled, but layout, labels, and glyphs may still render;
		// callers drop color inside this mode.
		return ModeInteractive
	}
	return ModeInteractive
}
