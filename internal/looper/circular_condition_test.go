package looper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The observed circular conditions (delivery audit 2026-11-17): Charlie T027
// and T066 wait for their own implementation to start and for their own
// deferral tag to disappear. The retained backlogs carry the same shape
// with the identifier in subject position, beside the honest external
// prerequisites that must stay quiet.
func TestCircularResumeCondition(t *testing.T) {
	cases := []struct {
		name      string
		task      Task
		circular  bool
		condition string
	}{
		{
			name:      "own identifier after waits for",
			task:      Task{ID: "T027", Status: "blocked", Blockers: []string{"Waits for T027 to start implementation and lose its deferred tag"}},
			circular:  true,
			condition: "Waits for T027 to start implementation and lose its deferred tag",
		},
		{
			name:      "own identifier in unblock condition",
			task:      Task{ID: "T066", Status: "blocked", UnblockWhen: "Waits for T066 to lose its deferred tag and start implementation"},
			circular:  true,
			condition: "Waits for T066 to lose its deferred tag and start implementation",
		},
		{
			name:     "own identifier after until",
			task:     Task{ID: "T040", Status: "blocked", Blockers: []string{"Blocked until T040 finishes its save work"}},
			circular: true,
		},
		{
			name:     "own identifier after depends on",
			task:     Task{ID: "T041", Status: "blocked", Blockers: []string{"Depends on T041 itself"}},
			circular: true,
		},
		{
			name:     "own identifier after requires",
			task:     Task{ID: "T042", Status: "blocked", UnblockWhen: "Requires T042 to become unblocked"},
			circular: true,
		},
		{
			// The literal shape the retained Charlie backlog carries today.
			name:     "own identifier starts implementation",
			task:     Task{ID: "T027", Status: "blocked", Blockers: []string{"Deferred: T027 starts implementation"}},
			circular: true,
		},
		{
			name:     "own identifier is unblocked",
			task:     Task{ID: "T066", Status: "blocked", Blockers: []string{"Deferred: T066 is unblocked (its dependency T015 has landed) and real minigame BrewedCup payloads reach Player"}},
			circular: true,
		},
		{
			name:     "own identifier in a conjunction of prerequisites",
			task:     Task{ID: "T050", Status: "blocked", Blockers: []string{"Deferred: T030 (Pourtland) is complete and T050 starts implementation"}},
			circular: true,
		},
		{
			name:     "own progress a few words after the identifier",
			task:     Task{ID: "T045", Status: "blocked", Blockers: []string{"Deferred: T045 art direction pass starts"}},
			circular: true,
		},
		{
			// The audit's T040 shape: the cue word sits directly before the
			// identifier, so the task itself is the required event.
			name:     "own identifier after when",
			task:     Task{ID: "T040", Status: "blocked", Blockers: []string{"Deferred: Resume when T040 unblocks and starts implementation; store the outro step beside quest data"}},
			circular: true,
		},
		{
			// The identifier labels the clause that follows; the external
			// device gates the task.
			name:     "progress word as a clause label",
			task:     Task{ID: "T052", Status: "blocked", Blockers: []string{"Deferred: T052 unblocks: an Android device with USB debugging is attached and the export preset is approved"}},
			circular: false,
		},
		{
			// The Ohya T040 shape: the condition requires the task to become
			// unblocked, which only the controller can do.
			name:     "own identifier is unblocked beside external work",
			task:     Task{ID: "T040", Status: "blocked", Blockers: []string{"Deferred: T040 is unblocked and docs/operations.md lands in the integrated tree"}},
			circular: true,
		},
		{
			// A possessive mention modifies another noun; the operator
			// provisioning the environment is the prerequisite.
			name:     "possessive mention of the unblock condition",
			task:     Task{ID: "T065", Status: "blocked", Blockers: []string{"Deferred: The operator provisions the relay environment that T065's unblock condition requires"}},
			circular: false,
		},
		{
			// The passive names the delivering agent: T118's push unblocks
			// the task, an external prerequisite.
			name:     "passive unblocked by another task",
			task:     Task{ID: "T130", Status: "blocked", Blockers: []string{"Deferred: Operator publishes the release (T130, itself unblocked by T118's push and CI-green evidence)"}},
			circular: false,
		},
		{
			name:     "another task is an ordinary prerequisite",
			task:     Task{ID: "T040", Status: "blocked", Blockers: []string{"Waits for T085 to finish its save work"}},
			circular: false,
		},
		{
			name:     "another task starting is an ordinary prerequisite",
			task:     Task{ID: "T041", Status: "blocked", Blockers: []string{"Deferred: T040 lands the section 18 auto-save system"}},
			circular: false,
		},
		{
			name:     "own identifier after an external prerequisite",
			task:     Task{ID: "T027", Status: "blocked", UnblockWhen: "After the operator grants access, T027 resumes implementation"},
			circular: false,
		},
		{
			name:     "own progress as the consequence of an external prerequisite",
			task:     Task{ID: "T027", Status: "blocked", UnblockWhen: "After the migration lands, T027 starts verification"},
			circular: false,
		},
		{
			name:     "own progress gated by a later external cue",
			task:     Task{ID: "T027", Status: "blocked", UnblockWhen: "T027 starts verification when the migration lands"},
			circular: false,
		},
		{
			name:     "own identifier as the consequence of another task",
			task:     Task{ID: "T027", Status: "blocked", Blockers: []string{"T085 must land before T027 starts"}},
			circular: false,
		},
		{
			name:     "own identifier after then",
			task:     Task{ID: "T055", Status: "blocked", Blockers: []string{"Deferred: T027 and T033 land, then T055 begins the playthrough"}},
			circular: false,
		},
		{
			name:     "reported outcome is not a required event",
			task:     Task{ID: "T077", Status: "blocked", Blockers: []string{"Latest worker report: T077 outcome verified complete at HEAD; no code change was needed"}},
			circular: false,
		},
		{
			name:     "own identifier in subject position",
			task:     Task{ID: "T027", Status: "blocked", UnblockWhen: "T027 waits for the save work to land"},
			circular: false,
		},
		{
			name:     "longer identifier that contains the own one",
			task:     Task{ID: "T027", Status: "blocked", Blockers: []string{"Waits for T0270 to finish"}},
			circular: false,
		},
		{
			// The audit's identifier-free T040 shape: the condition names no
			// task at all, and its subject is the task's own scope — "save
			// work" is the subject of "Auto-save system working per
			// section 18" — with its own resumption as the required event.
			name:     "own scope resuming without any identifier",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"Waits for save work to resume, or for dependent death-reload work to land"}},
			circular: true,
		},
		{
			// The retained Charlie T040 unblock wording: the identifier in
			// subject position with its own work resuming as the event.
			name:     "own identifier with its work resuming",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", UnblockWhen: "T040 auto-save work resumes; remove looper:deferred and set todo when this condition is met."},
			circular: true,
		},
		{
			// The deferral round only the task's own resumption ends, with
			// no identifier at all.
			name:     "own deferral round ending without any identifier",
			task:     Task{ID: "T027", Title: "Boss fight playable", Status: "blocked", Blockers: []string{"Waits for implementation to start and its deferred tag to be lost"}},
			circular: true,
		},
		{
			// An identifier-free condition whose subject matches no scope of
			// the task names other work, not the task itself.
			name:     "identifier-free subject outside the task scope",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"Waits for the art direction pass to resume"}},
			circular: false,
		},
		{
			name:     "identifier-free operator prerequisite stays quiet",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"Deferred: the operator grants access to the shared cluster"}},
			circular: false,
		},
		{
			// The cue governs the resumption, so an external prerequisite
			// satisfies the condition.
			name:     "identifier-free resumption governed by an external cue",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", UnblockWhen: "After the migration lands, the save work resumes"},
			circular: false,
		},
		{
			// The deferral boilerplate every deferred task's unblock
			// condition carries is an instruction, not a condition.
			name:     "deferral boilerplate alone stays quiet",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", UnblockWhen: "remove looper:deferred and set todo when this condition is met."},
			circular: false,
		},
		{
			// The quota hold's promise names the controller's retry, not the
			// task's own progress.
			name:     "quota retry promise stays quiet",
			task:     Task{ID: "T012", Title: "Auto-save system working per section 18", Status: "blocked", UnblockWhen: "retries automatically at 13:29:01+01:00"},
			circular: false,
		},
		{
			// A requirement word holds the task in requirement position, the
			// identifier-free reading of "until T040 finishes".
			name:     "identifier-free requirement position warns",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"Waits until the save work resumes"}},
			circular: true,
		},
		{
			// Subject words sit between the cued clause and the resumption
			// event; the operator grant is still the prerequisite.
			name:     "cue clause then own-scope subject resumes",
			task:     Task{ID: "T027", Title: "Auto-save system working per section 18", Status: "blocked", UnblockWhen: "After the operator grants access, the T027 work resumes"},
			circular: false,
		},
		{
			// The connector governs the clause even with subject words
			// between it and the event; T085 stays the prerequisite.
			name:     "connector then own-scope subject starts",
			task:     Task{ID: "T027", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"T085 must land before the T027 work starts"}},
			circular: false,
		},
		{
			// The identifier-free consequence of an external event, with the
			// task's own scope as the subject that follows.
			name:     "identifier-free connector consequence stays quiet",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"The migration lands, then the save work resumes"}},
			circular: false,
		},
		{
			name:     "identifier-free before consequence stays quiet",
			task:     Task{ID: "T040", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"The migration must land before the save work resumes"}},
			circular: false,
		},
		{
			// A conjunction without a cue or connector still names the
			// task's own start as an event, so the operator decides.
			name:     "conjunction without a cue still warns",
			task:     Task{ID: "T027", Title: "Auto-save system working per section 18", Status: "blocked", Blockers: []string{"The operator grants access, and the T027 work starts"}},
			circular: true,
		},
		{
			name:     "unblocked task",
			task:     Task{ID: "T027", Status: "todo", Blockers: []string{"Waits for T027 to start"}},
			circular: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := circularResumeCondition(tc.task)
			if tc.circular && got == "" {
				t.Fatalf("expected a circular condition for %s", tc.task.ID)
			}
			if !tc.circular && got != "" {
				t.Fatalf("reported %q as circular", got)
			}
			if tc.condition != "" && got != tc.condition {
				t.Fatalf("expected condition %q, got %q", tc.condition, got)
			}
		})
	}
}

// Doctor reports the circular condition as a warning in the legacy JSON
// document, in the version 2 document, and in plain text, without changing
// the task file or the saved state and without failing the command.
func TestDoctorWarnsOnCircularResumeCondition(t *testing.T) {
	blocked := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{
		{ID: "T101", Title: "self start", Priority: 2, Status: "blocked", Blockers: []string{"Waits for T101 to start implementation and lose its deferred tag"}},
		{ID: "T102", Title: "independent prerequisite", Priority: 2, Status: "blocked", Blockers: []string{"Waits for T085 to finish its save work"}},
		{ID: "T103", Title: "honest deferral", Priority: 2, Status: "blocked", Tags: []string{deferredTaskTag}, UnblockWhen: "After the operator grants access, T103 resumes implementation"},
		{ID: "T104", Title: "deferred on itself", Priority: 2, Status: "blocked", Tags: []string{deferredTaskTag}, Blockers: []string{"Waits for T104 to lose its deferred tag and start implementation"}},
		{ID: "T105", Title: "subject shape", Priority: 2, Status: "blocked", Blockers: []string{"Deferred: T105 starts implementation"}},
		{ID: "T106", Title: "Auto-save system working per section 18", Priority: 2, Status: "blocked", Blockers: []string{"Waits for save work to resume, or for dependent death-reload work to land"}},
		{ID: "T107", Title: "Auto-save system working per section 18", Priority: 2, Status: "blocked", UnblockWhen: "Deferred: the operator grants access to the shared cluster"},
	}}

	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Command: "doctor", JSON: true}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(&State{Version: 1, Backlog: blocked}); err != nil {
		t.Fatal(err)
	}
	store.Close()
	stateBefore, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Run("legacy json", func(t *testing.T) {
		var out bytes.Buffer
		if code := doctorJSON(c, &out); code != exitOK {
			t.Fatalf("a warning must keep exit code %d, got %d: %s", exitOK, code, out.String())
		}
		var doc struct {
			Findings []HealthFinding `json:"findings"`
		}
		if err = json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		byTask := map[string]HealthFinding{}
		for _, f := range doc.Findings {
			if f.Code == findingTaskCircular {
				if _, seen := byTask[f.TaskID]; seen {
					t.Fatalf("two circular findings for %s", f.TaskID)
				}
				byTask[f.TaskID] = f
			}
		}
		conditions := map[string]string{
			"T101": "Waits for T101 to start implementation and lose its deferred tag",
			"T104": "Waits for T104 to lose its deferred tag and start implementation",
			"T105": "Deferred: T105 starts implementation",
			"T106": "Waits for save work to resume, or for dependent death-reload work to land",
		}
		for id, condition := range conditions {
			f, ok := byTask[id]
			if !ok {
				t.Fatalf("missing circular finding for %s: %s", id, out.String())
			}
			if f.Severity != "warning" {
				t.Fatalf("%s finding severity %q", id, f.Severity)
			}
			if !strings.Contains(f.Message, condition) {
				t.Fatalf("%s finding must quote the condition: %+v", id, f)
			}
			if !strings.Contains(f.Action, "independent prerequisite") {
				t.Fatalf("%s finding must guide the operator: %+v", id, f)
			}
		}
		for _, id := range []string{"T102", "T103", "T107"} {
			if _, ok := byTask[id]; ok {
				t.Fatalf("unexpected circular finding for %s", id)
			}
		}
	})

	t.Run("version 2 json", func(t *testing.T) {
		c.Contract = ContractVersionV2
		var out bytes.Buffer
		if code := doctorJSON(c, &out); code != exitOK {
			t.Fatalf("version 2 exits 0 for findings, got %d: %s", code, out.String())
		}
		var doc struct {
			Findings []doctorFindingView `json:"findings"`
		}
		if err = json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatal(err)
		}
		circular := map[string]doctorFindingView{}
		for _, f := range doc.Findings {
			if f.Code == findingTaskCircular {
				circular[f.TaskID] = f
			}
		}
		if len(circular) != 4 {
			t.Fatalf("expected the four circular findings, got %+v", circular)
		}
		// The operator and deferral classes are expected waiting, but a
		// condition only the blocked task itself can satisfy never is: the
		// deferral tag on T104 must not downgrade its warning.
		if f := circular["T104"]; f.Severity != "warning" {
			t.Fatalf("T104 circular finding downgraded to %q: %+v", f.Severity, f)
		}
		if f := circular["T101"]; f.Severity != "warning" {
			t.Fatalf("T101 circular finding severity %q", f.Severity)
		}
		for _, f := range doc.Findings {
			if f.TaskID == "T103" && f.Severity != "info" {
				t.Fatalf("the honest deferral stays informational: %+v", f)
			}
		}
	})

	stateAfter, err := os.ReadFile(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stateBefore, stateAfter) {
		t.Fatal("doctor changed the saved state")
	}

	t.Run("plain text", func(t *testing.T) {
		bin := filepath.Join(t.TempDir(), "agent.sh")
		if err = os.WriteFile(bin, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
		todo := filepath.Join(c.Root, "to-do.json")
		if err = writeJSON(todo, blocked); err != nil {
			t.Fatal(err)
		}
		text := Config{Root: c.Root, BaseDir: c.BaseDir, Todo: todo, Agent: "claude", ReviewAgent: "claude", ClaudeBin: bin, Plain: true}
		before, err := os.ReadFile(todo)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if code := doctor(text, &out); code != exitOK {
			t.Fatalf("a warning must keep exit code %d, got %d: %s", exitOK, code, out.String())
		}
		for _, id := range []string{"T101", "T104"} {
			if !strings.Contains(out.String(), "Warning: task "+id+" requires itself to resume: Waits for "+id) {
				t.Fatalf("missing plain warning for %s:\n%s", id, out.String())
			}
		}
		if !strings.Contains(out.String(), "Warning: task T105 requires itself to resume: Deferred: T105 starts implementation") {
			t.Fatalf("missing plain warning for the subject shape:\n%s", out.String())
		}
		if !strings.Contains(out.String(), "Warning: task T106 requires itself to resume: Waits for save work to resume") {
			t.Fatalf("missing plain warning for the identifier-free shape:\n%s", out.String())
		}
		for _, id := range []string{"T102", "T103", "T107"} {
			if strings.Contains(out.String(), "task "+id+" requires itself") {
				t.Fatalf("unexpected plain warning for %s:\n%s", id, out.String())
			}
		}
		if !strings.Contains(out.String(), "Fix: record an independent prerequisite or correct the task state in") {
			t.Fatalf("missing guidance:\n%s", out.String())
		}
		after, err := os.ReadFile(todo)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("doctor changed the task file")
		}
	})
}

// The compact status document carries the same finding, and the deferral
// class does not downgrade it there either.
func TestCompactStatusKeepsCircularWarning(t *testing.T) {
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Command: "status"}
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s := &State{Version: 1, Backlog: Backlog{Tasks: []Task{
		{ID: "T005", Title: "deferred on itself", Priority: 2, Status: "blocked",
			Tags:        []string{deferredTaskTag},
			Blockers:    []string{"Waits for T005 to lose its deferred tag and start implementation"},
			UnblockWhen: "Waits for T005 to lose its deferred tag and start implementation"},
	}}}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	snap, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	ev := &statusEvidence{snap: snap, findings: diagnose(Observation{}, snap.State)}
	var found bool
	for _, f := range statusFindingsFor(ev, statusFits[0]) {
		if f.Code != findingTaskCircular {
			continue
		}
		found = true
		if f.TaskID != "T005" || f.Severity != "warning" {
			t.Fatalf("downgraded circular finding: %+v", f)
		}
		if f.ReasonClass != blockClassDeferred {
			t.Fatalf("the deferral context must stay beside the warning: %+v", f)
		}
	}
	if !found {
		t.Fatal("compact status omitted the circular finding")
	}
}
