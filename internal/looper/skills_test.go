package looper

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The agent skills teach the public workflows, so their command examples
// stay executable: every `looper ...` line in a skill code block parses, and
// every command a skill names is a command this executable serves (AX-026).
// The check is structural — a skill cannot pass by wording alone, and a
// renamed or removed command fails the skills that still teach it.
func TestSkillCommandsParse(t *testing.T) {
	skills := filepath.Join("..", "..", "skills")
	entries, err := os.ReadDir(skills)
	if err != nil {
		t.Skipf("skills directory is absent: %v", err)
	}
	known := map[string]bool{}
	for _, name := range clientCommands {
		known[name] = true
	}
	// Legacy flag spellings a skill may name in prose.
	commandWord := regexp.MustCompile(`\blooper ([a-z][a-z0-9-]*)`)
	taught := 0
	for _, entry := range entries {
		path := filepath.Join(skills, entry.Name(), "SKILL.md")
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := string(b)
		for _, match := range commandWord.FindAllStringSubmatch(text, -1) {
			if !known[match[1]] {
				t.Errorf("%s teaches %q, which the client does not serve", entry.Name(), match[1])
			}
		}
		for _, block := range fencedBlocks(text) {
			for _, line := range strings.Split(block, "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 || fields[0] != "looper" {
					continue
				}
				taught++
				if _, err := Parse(fields[1:]); err != nil {
					t.Errorf("%s teaches %q, which does not parse: %v", entry.Name(), line, err)
				}
			}
		}
	}
	if taught == 0 {
		t.Fatal("no skill teaches a looper command")
	}
}

// fencedBlocks returns the contents of the fenced code blocks a skill carries.
func fencedBlocks(text string) []string {
	var blocks []string
	var current []string
	open := false
	for _, line := range strings.Split(text, "\n") {
		switch {
		case strings.HasPrefix(line, "```") && !open:
			open = true
			current = current[:0]
		case strings.HasPrefix(line, "```") && open:
			open = false
			blocks = append(blocks, strings.Join(current, "\n"))
		case open:
			current = append(current, line)
		}
	}
	return blocks
}

// The diagnosis skill teaches the compact workflows the agent-experience plan
// delivered: bounded observation, bounded continuation, finite waits, focused
// inspection, receipt queries, and the recovery preview. The check reads the
// taught invocations, not the prose around them.
func TestDiagnoseSkillTeachesTheContractWorkflows(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "skills", "looper-diagnose", "SKILL.md"))
	if err != nil {
		t.Skipf("diagnose skill is absent: %v", err)
	}
	text := string(b)
	for _, needed := range []string{
		"status --contract v2 --json",
		"--after",
		"wait --contract v2",
		"requests --contract v2 --json",
		"inspect T054 --contract v2 --json",
		"recovery --preview --contract v2 --json",
		"--kind failure",
	} {
		if !strings.Contains(text, needed) {
			t.Errorf("the diagnose skill does not teach %q", needed)
		}
	}
	// The polling-era invocations are gone.
	if strings.Contains(text, "looper status --json") || strings.Contains(text, "looper events --json --after") {
		t.Error("the diagnose skill still teaches an unbounded or polling-era invocation")
	}
}
