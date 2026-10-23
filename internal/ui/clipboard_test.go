package ui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCopyCompleteTaskAndLatestBlocker(t *testing.T) {
	tsk := TaskView{ID: "T032", Title: "Native validation", Status: "blocked", Revision: "new", Blockers: []string{strings.Repeat("evidence ", 100)}, UnblockWhen: "macOS host available"}
	m := model{mode: ModeInspect, width: 30, snap: &ViewSnapshot{Project: "melt", Root: "/home/agent/melt", Session: "run-1", EventCursor: 772, Tasks: []TaskView{tsk}}}
	m.workspace.detailID = tsk.ID
	m.workspace.detail = &TaskDetail{Task: TaskView{ID: tsk.ID, Revision: "old", Details: "obsolete purpose", Blockers: []string{"old blocker"}}}
	text := m.copyText()
	for _, want := range []string{"Diagnose this Looper run", "Project root: /home/agent/melt", "Session: run-1", "looper status --json", "looper doctor --json", "looper inspect 'T032' --json", "looper events --json --after 772"} {
		if !strings.Contains(text, want) {
			t.Fatalf("handoff missing %q: %s", want, text)
		}
	}
	if !strings.Contains(text, tsk.Blockers[0]) || !strings.Contains(text, tsk.UnblockWhen) || strings.Contains(text, "obsolete") || strings.Contains(text, "old blocker") {
		t.Fatal(text)
	}
	m.workspace.detail.Task.Revision = "new"
	m.workspace.detail.Task.Details = "current purpose"
	if !strings.Contains(m.copyText(), "current purpose") {
		t.Fatal("missing details")
	}
}
func TestCopyViewsAndTerminalSequence(t *testing.T) {
	m := model{mode: ModeLog, logPath: "log", logText: "\x1b[31mred\x1b[0m\x00\nlast line"}
	if text := m.copyText(); !strings.Contains(text, "red\nlast line") || strings.Contains(text, "\x1b") {
		t.Fatal(text)
	}
	m.mode = ModeDiff
	m.diffText = "+complete change"
	if !strings.Contains(m.copyText(), m.diffText) {
		t.Fatal("missing diff")
	}
	var out bytes.Buffer
	c := clipboardCommand{text: "copied\nstate"}
	c.SetStdout(&out)
	if err := c.Run(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), base64.StdEncoding.EncodeToString([]byte(c.text))) {
		t.Fatal("missing encoded payload")
	}
}
func TestCopyKeyAndFeedback(t *testing.T) {
	m := model{}
	_, cmd := m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if cmd == nil {
		t.Fatal("copy key has no command")
	}
	next, _ := m.Update(clipboardResult{err: errors.New("write failed")})
	if !strings.Contains(next.(model).err, "write failed") {
		t.Fatal("missing error")
	}
	next, _ = next.(model).Update(clipboardResult{})
	if next.(model).err != "" || !strings.Contains(next.(model).message, "sent") {
		t.Fatal("missing success notice")
	}
	m.initWorkspace()
	m.workspace.searching = true
	m.workspace.search.Focus()
	next, cmd = m.keys(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'c'}})
	if next.(model).workspace.search.Value() != "c" {
		t.Fatal("copy key intercepted search")
	}
}

func TestDeferredTaskViewAndReviewReason(t *testing.T) {
	task := TaskView{ID: "T1", Status: "blocked", Deferred: true}
	if taskPhase(task) != "deferred" {
		t.Fatal("missing deferred phase")
	}
	v := &ViewSnapshot{DispatchReason: "Milestone review is due before dispatch", Counters: ViewCounters{Ready: 1}}
	if activityLabel(v, false) != v.DispatchReason {
		t.Fatal("hidden dispatch reason")
	}
}

func TestHandoffBoundsLargeEvidence(t *testing.T) {
	text := strings.Repeat("前", 13000) + "latest failure"
	got := handoffExcerpt(text, true)
	if !strings.HasSuffix(got, "latest failure") || !strings.Contains(got, "Earlier content omitted") {
		t.Fatal("missing latest log evidence")
	}
	got = handoffExcerpt(text, false)
	if !strings.HasPrefix(got, "前") || !strings.Contains(got, "Remaining content omitted") {
		t.Fatal("missing diff excerpt marker")
	}
}
