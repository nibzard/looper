package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

func protocolEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	e := &Engine{
		Dir: dir, Output: io.Discard, Store: store,
		Config:     Config{ProfilePath: filepath.Join(dir, "profile.json"), Jobs: 4, ReviewEvery: 5},
		controller: &ControllerInfo{Version: 1, Session: "run-test", PID: os.Getpid(), Mode: ControllerModeForeground},
		acks:       map[string]SettingsAck{},
		viewNotify: make(chan ui.ViewSnapshot, 1),
		capacity:   4,
	}
	return e
}

func request(e *Engine, fields map[string]any, digest string) *SettingsRequest {
	raw := map[string]json.RawMessage{}
	for k, v := range fields {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	return &SettingsRequest{Version: SettingsRequestVersion, ID: identifier("req"), Session: e.controller.Session, ProfileDigest: digest, Fields: raw}
}

func TestApplySettingsRequestApplied(t *testing.T) {
	e := protocolEngine(t)
	if err := SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Agent: "codex", Jobs: 2, ReviewEvery: func() *int { v := 5; return &v }()}); err != nil {
		t.Fatal(err)
	}
	digest := profileDigest(e.Config.ProfilePath)
	req := request(e, map[string]any{"agent": "claude", "jobs": 2, "model": "claude-x"}, digest)
	ack := e.applySettingsRequest(req)
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	p, err := LoadProfile(e.Config.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if p.Agent != "claude" || p.Model != "claude-x" {
		t.Fatalf("profile not saved: %+v", p)
	}
	if e.Config.Agent != "claude" || e.Config.Model != "claude-x" {
		t.Fatalf("live settings not applied: %+v", e.Config)
	}
	if ack.Outcomes["agent"] != "saved; applied" {
		t.Fatalf("outcome %q", ack.Outcomes["agent"])
	}
	if ack.ProfileDigest == "" || ack.ProfileDigest == digest {
		t.Fatal("resulting digest missing")
	}
	// The settings event names the request identifier.
	events := storedEvents(t, e)
	found := false
	for _, ev := range events {
		if ev.Kind == "settings" && strings.Contains(ev.Message, req.ID) {
			found = true
		}
	}
	if !found {
		t.Fatal("settings event missing the request identifier")
	}
}

func storedEvents(t *testing.T, e *Engine) []Event {
	t.Helper()
	events, err := e.Store.Events(0)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestApplySettingsRequestConflicts(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1})
	digest := profileDigest(e.Config.ProfilePath)
	// Wrong digest: a concurrent manual edit.
	ack := e.applySettingsRequest(request(e, map[string]any{"jobs": 3}, "stale-digest"))
	if ack.Status != AckConflict {
		t.Fatalf("expected conflict, got %s", ack.Status)
	}
	// Another session: never replayed into this controller.
	other := request(e, map[string]any{"jobs": 3}, digest)
	other.Session = "run-older"
	ack = e.applySettingsRequest(other)
	if ack.Status != AckStale {
		t.Fatalf("expected stale, got %s", ack.Status)
	}
	// Unknown and invalid fields.
	ack = e.applySettingsRequest(request(e, map[string]any{"nonsense": 1}, digest))
	if ack.Status != AckError {
		t.Fatalf("unknown field accepted: %s", ack.Status)
	}
	ack = e.applySettingsRequest(request(e, map[string]any{"agent": "gemini"}, digest))
	if ack.Status != AckError {
		t.Fatalf("invalid agent accepted: %s", ack.Status)
	}
	// Duplicate identifiers return the recorded acknowledgement.
	first := request(e, map[string]any{"jobs": 3}, digest)
	firstAck := e.applySettingsRequest(first)
	digest = profileDigest(e.Config.ProfilePath)
	second := request(e, map[string]any{"jobs": 8}, digest)
	second.ID = first.ID
	if got := e.applySettingsRequest(second); got.Status != firstAck.Status || got.ProfileDigest != firstAck.ProfileDigest {
		t.Fatalf("duplicate id not answered from the record: %+v", got)
	}
}

func TestApplySettingsRequestReset(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Agent: "claude"})
	e.Config.Agent = "claude"
	digest := profileDigest(e.Config.ProfilePath)
	req := request(e, nil, digest)
	req.Reset = true
	ack := e.applySettingsRequest(req)
	if ack.Status != AckReset {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	if _, err := os.Stat(e.Config.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("reset must remove the profile")
	}
	// Effective settings stay unchanged until restart.
	if e.Config.Agent != "claude" {
		t.Fatal("reset changed effective settings mid-run")
	}
}

func TestApplySettingsRequestDeferredAndOverridden(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, VerifyCommand: "go test ./...", ReviewEvery: func() *int { v := 3; return &v }()})
	digest := profileDigest(e.Config.ProfilePath)
	req := request(e, map[string]any{"verify_command": "make test", "review_every": 9}, digest)
	ack := e.applySettingsRequest(req)
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	if e.Config.Verify != "" || e.Config.ReviewEvery != 5 {
		t.Fatalf("deferred settings applied live: %+v", e.Config)
	}
	if ack.Outcomes["verify_command"] != "saved; applies on the next controller run" {
		t.Fatalf("outcome %q", ack.Outcomes["verify_command"])
	}
	// A flag-authoritative field is saved but reported overridden.
	e2 := protocolEngine(t)
	SaveProfile(e2.Config.ProfilePath, &Profile{Version: 1})
	e2.Config.Agent = "codex"
	e2.Config.Sources.Agent = SourceFlag
	req = request(e2, map[string]any{"agent": "claude"}, profileDigest(e2.Config.ProfilePath))
	ack = e2.applySettingsRequest(req)
	if ack.Status != AckApplied || e2.Config.Agent != "codex" {
		t.Fatalf("flag authority broken: %+v %+v", ack, e2.Config)
	}
	if ack.Outcomes["agent"] == "saved; applied" {
		t.Fatal("overridden outcome missing")
	}
}

// A live edit that clears the agent value applies the shared default agent,
// keeps the profile field empty, and leaves explicit selections unchanged.
func TestLiveAgentDefaultAndExplicitSelections(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Agent: "codex"})
	digest := profileDigest(e.Config.ProfilePath)
	ack := e.applySettingsRequest(request(e, map[string]any{"agent": ""}, digest))
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	if e.Config.Agent != defaultAgent {
		t.Fatalf("cleared agent must apply the shared default %s, got %q", defaultAgent, e.Config.Agent)
	}
	if ack.Outcomes["agent"] != "saved; applied" {
		t.Fatalf("outcome %q", ack.Outcomes["agent"])
	}
	p, err := LoadProfile(e.Config.ProfilePath)
	if err != nil || p.Agent != "" {
		t.Fatalf("cleared agent must stay empty in the profile: %v %+v", err, p)
	}
	// Explicit selections apply live exactly as saved.
	for _, agent := range []string{"codex", "claude", "zcode"} {
		digest = profileDigest(e.Config.ProfilePath)
		ack = e.applySettingsRequest(request(e, map[string]any{"agent": agent}, digest))
		if ack.Status != AckApplied || e.Config.Agent != agent {
			t.Fatalf("explicit agent %s broken: %s %+v", agent, ack.Status, e.Config)
		}
	}
}

// Flag and environment sources stay authoritative over any live agent change,
// including the cleared value that resolves to the shared default.
func TestLiveAgentOverridesStayAuthoritative(t *testing.T) {
	for _, source := range []Source{SourceEnv, SourceFlag} {
		e := protocolEngine(t)
		SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Agent: "zcode"})
		e.Config.Agent = "codex"
		e.Config.Sources.Agent = source
		for _, next := range []string{"claude", ""} {
			ack := e.applySettingsRequest(request(e, map[string]any{"agent": next}, profileDigest(e.Config.ProfilePath)))
			if ack.Status != AckApplied {
				t.Fatalf("source %v value %q: status %s: %s", source, next, ack.Status, ack.Message)
			}
			if e.Config.Agent != "codex" {
				t.Fatalf("source %v must stay authoritative for %q: %q", source, next, e.Config.Agent)
			}
			// The outcome names the saved value the override wins over.
			saved := orDefault(next, defaultAgent)
			if want := "saved; overridden by flag or environment for this run (" + saved + ")"; ack.Outcomes["agent"] != want {
				t.Fatalf("source %v value %q outcome %q", source, next, ack.Outcomes["agent"])
			}
		}
	}
}

// After a reset request the saved profile is gone, so the next form and the
// next run fall back to the shared agent default.
func TestApplySettingsRequestResetReturnsToDefaultAgent(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Agent: "codex"})
	e.Config.Agent = "codex"
	req := request(e, nil, profileDigest(e.Config.ProfilePath))
	req.Reset = true
	ack := e.applySettingsRequest(req)
	if ack.Status != AckReset {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	// Effective settings stay unchanged until restart.
	if e.Config.Agent != "codex" {
		t.Fatal("reset changed effective settings mid-run")
	}
	profile, err := LoadProfile(e.Config.ProfilePath)
	if err != nil || profile != nil {
		t.Fatalf("reset profile must load as nil: %v %+v", err, profile)
	}
	if s := settingsFromProfile(profile); s.Agent != defaultAgent {
		t.Fatalf("post-reset form must default to %s, got %q", defaultAgent, s.Agent)
	}
}

func TestApplySettingsRequestJobsLimits(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Jobs: 4})
	digest := profileDigest(e.Config.ProfilePath)
	// Decrease: capacity follows down.
	e.applySettingsRequest(request(e, map[string]any{"jobs": 2}, digest))
	if e.Config.Jobs != 2 || e.capacity != 2 {
		t.Fatalf("decrease broken: jobs %d capacity %d", e.Config.Jobs, e.capacity)
	}
	// Backoff shrink then increase: backoff limit is preserved.
	digest = profileDigest(e.Config.ProfilePath)
	e.capacity = 1
	e.applySettingsRequest(request(e, map[string]any{"jobs": 6}, digest))
	if e.Config.Jobs != 6 || e.capacity != 1 {
		t.Fatalf("increase must preserve backoff: jobs %d capacity %d", e.Config.Jobs, e.capacity)
	}
	// Auto mode resolves and marks auto.
	digest = profileDigest(e.Config.ProfilePath)
	e.applySettingsRequest(request(e, map[string]any{"jobs": 0}, digest))
	if !e.Config.JobsAuto || e.Config.Jobs < 1 {
		t.Fatalf("auto mode broken: %+v", e.Config)
	}
}

func TestProcessSettingsRequestsEndToEnd(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1})
	digest := profileDigest(e.Config.ProfilePath)
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "r-1.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "r-1", Session: e.controller.Session,
		ProfileDigest: digest, Fields: map[string]json.RawMessage{"jobs": json.RawMessage("3")},
	}); err != nil {
		t.Fatal(err)
	}
	// An invalid envelope gets an error ack and never stops the controller.
	if err := os.WriteFile(filepath.Join(requestDir(e.Dir), "bad.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	e.processSettingsRequests()
	if e.err != nil {
		t.Fatalf("controller failed on invalid request: %v", e.err)
	}
	ack, err := LoadAck(e.Dir, "r-1")
	if err != nil || ack.Status != AckApplied {
		t.Fatalf("ack missing: %+v %v", ack, err)
	}
	if _, err = os.Stat(filepath.Join(processingDir(e.Dir), "r-1.json")); !os.IsNotExist(err) {
		t.Fatal("claimed request not removed")
	}
	if ack, err = LoadAck(e.Dir, "bad"); err != nil || ack.Status != AckError {
		t.Fatalf("invalid envelope not acked: %+v %v", ack, err)
	}
}

func LoadAck(dir, id string) (*SettingsAck, error) {
	var ack SettingsAck
	if err := readJSON(filepath.Join(ackDir(dir), id+".json"), &ack); err != nil {
		return nil, err
	}
	return &ack, nil
}

func TestClearSessionFilesKeepsPlanImports(t *testing.T) {
	dir := t.TempDir()
	writeJSON(filepath.Join(requestDir(dir), "old-settings.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "a", Type: RequestTypeSettings, Session: "old", SubmitterPID: deadSubmitterPID(t)})
	writeJSON(filepath.Join(requestDir(dir), "keep-import.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "b", Type: RequestTypePlanImport, Session: "old"})
	writeJSON(filepath.Join(processingDir(dir), "stuck.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "c", Type: RequestTypeSettings, Session: "old", SubmitterPID: deadSubmitterPID(t)})
	old := time.Now().Add(-2 * staleAckAge)
	if err := writeJSON(filepath.Join(ackDir(dir), "old-ack.json"), SettingsAck{Version: 1, ID: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(ackDir(dir), "old-ack.json"), old, old); err != nil {
		t.Fatal(err)
	}
	clearSessionFiles(dir)
	if _, err := os.Stat(filepath.Join(requestDir(dir), "keep-import.json")); err != nil {
		t.Fatal("plan-import request must survive session cleanup")
	}
	if _, err := os.Stat(filepath.Join(requestDir(dir), "old-settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings request from a gone submitter kept")
	}
	if _, err := os.Stat(filepath.Join(processingDir(dir), "stuck.json")); !os.IsNotExist(err) {
		t.Fatal("stuck processing request from a gone submitter kept")
	}
	if _, err := os.Stat(filepath.Join(ackDir(dir), "old-ack.json")); !os.IsNotExist(err) {
		t.Fatal("old acknowledgement kept")
	}
}

// deadSubmitterPID returns the process ID of a child this test already
// reaped, so a request envelope bound to it names a submitter that is
// verifiably gone.
func deadSubmitterPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "5")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn a child process: %v", err)
	}
	pid := cmd.Process.Pid
	cmd.Process.Kill()
	cmd.Wait()
	return pid
}

// The sweep removes a request only when its recorded submitter is verifiably
// gone. A live submitter's envelope, and one without a recorded submitter,
// stay so the controller that just started answers them.
func TestClearSessionFilesSweepsOnlyDeadSubmitters(t *testing.T) {
	dir := t.TempDir()
	live := SettingsRequest{Version: SettingsRequestVersion, ID: "live", Type: RequestTypeSettings, Session: "old"}
	live.SubmitterPID = os.Getpid()
	if identity, err := runIdentityCommand(live.SubmitterPID); err == nil {
		live.SubmitterIdentity = identity
	}
	writeJSON(filepath.Join(requestDir(dir), "live.json"), live)
	writeJSON(filepath.Join(requestDir(dir), "unbound.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "unbound", Type: RequestTypeSettings, Session: "old"})
	writeJSON(filepath.Join(requestDir(dir), "gone.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "gone", Type: RequestTypeSettings, Session: "old", SubmitterPID: deadSubmitterPID(t)})
	// A recorded identity that no longer matches a live process ID is a
	// reused ID: the recorded submitter is gone.
	writeJSON(filepath.Join(requestDir(dir), "reused.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "reused", Type: RequestTypeSettings, Session: "old", SubmitterPID: os.Getpid(), SubmitterIdentity: "1"})
	clearSessionFiles(dir)
	for _, id := range []string{"live", "unbound"} {
		if _, err := os.Stat(filepath.Join(requestDir(dir), id+".json")); err != nil {
			t.Fatalf("request with an unverifiable or live submitter must stay: %s", id)
		}
	}
	for _, id := range []string{"gone", "reused"} {
		if _, err := os.Stat(filepath.Join(requestDir(dir), id+".json")); !os.IsNotExist(err) {
			t.Fatalf("request from a verifiably gone submitter must be removed: %s", id)
		}
	}
}

// A claim the previous controller left in processing/ returns to requests/
// when its submitter is alive and no acknowledgement exists, so a boundary
// answers it. A claim whose acknowledgement exists is complete: the envelope
// is dropped and the retained acknowledgement stays readable.
func TestClearSessionFilesReturnsLiveClaims(t *testing.T) {
	dir := t.TempDir()
	writeJSON(filepath.Join(processingDir(dir), "claimed.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "claimed", Type: RequestTypeSettings, Session: "old",
		SubmitterPID: os.Getpid(),
	})
	writeJSON(filepath.Join(processingDir(dir), "answered.json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: "answered", Type: RequestTypeRequeue, Session: "old",
		SubmitterPID: os.Getpid(),
	})
	writeJSON(filepath.Join(ackDir(dir), "answered.json"), SettingsAck{Version: SettingsRequestVersion, ID: "answered", Status: AckApplied})
	writeJSON(filepath.Join(processingDir(dir), "import.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "import", Type: RequestTypePlanImport, Session: "old"})
	clearSessionFiles(dir)
	if _, err := os.Stat(filepath.Join(requestDir(dir), "claimed.json")); err != nil {
		t.Fatal("a live submitter's stranded claim must return to the queue")
	}
	if _, err := os.Stat(filepath.Join(processingDir(dir), "claimed.json")); !os.IsNotExist(err) {
		t.Fatal("the stranded claim must leave processing/")
	}
	if _, err := os.Stat(filepath.Join(processingDir(dir), "answered.json")); !os.IsNotExist(err) {
		t.Fatal("an answered claim must not return to the queue")
	}
	if _, err := os.Stat(filepath.Join(requestDir(dir), "answered.json")); !os.IsNotExist(err) {
		t.Fatal("an answered claim must not be resubmitted")
	}
	if ack, ok := readRequestAck(dir, "answered"); !ok || ack.Status != AckApplied {
		t.Fatalf("the written acknowledgement must stay readable: %+v, %v", ack, ok)
	}
	if _, err := os.Stat(filepath.Join(processingDir(dir), "import.json")); err != nil {
		t.Fatal("a plan-import claim must stay for import recovery")
	}
}

// An acknowledgement younger than staleAckAge stays: a submitter may not
// have read its answer when the next controller starts. Plan-import
// acknowledgements always stay, whatever their age.
func TestClearSessionFilesKeepsYoungAcks(t *testing.T) {
	dir := t.TempDir()
	writeJSON(filepath.Join(ackDir(dir), "young.json"), SettingsAck{Version: SettingsRequestVersion, ID: "young", Status: AckApplied})
	if err := writeJSON(filepath.Join(ackDir(dir), "old.json"), SettingsAck{Version: SettingsRequestVersion, ID: "old", Status: AckApplied}); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * staleAckAge)
	if err := os.Chtimes(filepath.Join(ackDir(dir), "old.json"), stale, stale); err != nil {
		t.Fatal(err)
	}
	writeJSON(filepath.Join(ackDir(dir), "import.json"), planImportAck{Version: SettingsRequestVersion, Type: RequestTypePlanImport, ID: "import", Status: ImportQueued})
	if err := os.Chtimes(filepath.Join(ackDir(dir), "import.json"), stale, stale); err != nil {
		t.Fatal(err)
	}
	clearSessionFiles(dir)
	if _, err := os.Stat(filepath.Join(ackDir(dir), "young.json")); err != nil {
		t.Fatal("a young acknowledgement may still be unread")
	}
	if _, err := os.Stat(filepath.Join(ackDir(dir), "old.json")); !os.IsNotExist(err) {
		t.Fatal("an aged acknowledgement must be collected")
	}
	if _, err := os.Stat(filepath.Join(ackDir(dir), "import.json")); err != nil {
		t.Fatal("a plan-import acknowledgement must always stay")
	}
}

// The acceptance core: a settings request submitted just before a controller
// restart receives its acknowledgement or a conflict, never a silent
// deletion. The startup sweep keeps the in-flight envelope because its
// submitter is alive, and the controller that starts next answers it with
// the stale-session acknowledgement.
func TestSettingsRequestSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	type result struct {
		ack *SettingsAck
		err error
	}
	done := make(chan result, 1)
	go func() {
		ack, err := submitSettingsRequest(dir, SettingsRequest{
			Version: SettingsRequestVersion, Session: "run-first",
			Fields: map[string]json.RawMessage{"agent": json.RawMessage(`"claude"`)},
		}, 10*time.Second)
		done <- result{ack, err}
	}()
	envelope := ""
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(requestDir(dir))
		if err == nil {
			for _, entry := range entries {
				if strings.HasSuffix(entry.Name(), ".json") {
					envelope = entry.Name()
				}
			}
			if envelope != "" {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if envelope == "" {
		t.Fatal("the submitter never published its request")
	}
	// The next controller starts: its startup sweep must keep the envelope.
	clearSessionFiles(dir)
	if names := queuedJSONNames(t, dir); len(names) != 1 || names[0] != envelope {
		t.Fatalf("an in-flight request was swept: %v", names)
	}
	next := protocolEngineOn(t, dir, "run-next")
	next.processSettingsRequests()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("the submitter never received an answer: %v", r.err)
		}
		if r.ack == nil || r.ack.Status != AckStale {
			t.Fatalf("the restart must answer with a conflict, got %+v", r.ack)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the submitter is still waiting after the restart")
	}
	if names := queuedJSONNames(t, dir); len(names) != 0 {
		t.Fatalf("the answered request must leave the queue, got %v", names)
	}
}

// protocolEngineOn builds a protocol engine on an existing runtime directory
// with a chosen controller session, for tests that play more than one
// session against one request queue.
func protocolEngineOn(t *testing.T, dir, session string) *Engine {
	t.Helper()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return &Engine{
		Dir: dir, Output: io.Discard, Store: store,
		Config:     Config{ProfilePath: filepath.Join(dir, "profile.json"), Jobs: 4, ReviewEvery: 5},
		controller: &ControllerInfo{Version: 1, Session: session, PID: os.Getpid(), Mode: ControllerModeForeground},
		acks:       map[string]SettingsAck{}, viewNotify: make(chan ui.ViewSnapshot, 1),
		capacity: 4,
	}
}

// An envelope refused at claim time stays while its error acknowledgement
// cannot be written; the claim reports the failure so the boundary sleeps,
// and the next pass answers the envelope once the obstacle is gone.
func TestClaimKeepsRefusedEnvelopeWhenAckWriteFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "acks"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(requestDir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(requestDir(dir), "broken.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := claimNextRequest(dir, RequestTypeSettings); err == nil {
		t.Fatal("an unwritable acknowledgement must fail the claim")
	} else if !strings.Contains(err.Error(), "stays queued") {
		t.Fatalf("the claim error must explain the keep: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "broken.json")); statErr != nil {
		t.Fatal("a refused envelope must stay while its acknowledgement cannot be written")
	}
	if err := os.Remove(filepath.Join(dir, "acks")); err != nil {
		t.Fatal(err)
	}
	if req, _, err := claimNextRequest(dir, RequestTypeSettings); req != nil || err != nil {
		t.Fatalf("the retry must stop at the kept envelope: %+v, %v", req, err)
	}
	ack, ok := readRequestAck(dir, "broken")
	if !ok || ack.Status != AckError || ack.Message == "" {
		t.Fatalf("the retried refusal must carry its reason: %+v, %v", ack, ok)
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "broken.json")); !os.IsNotExist(statErr) {
		t.Fatal("the answered envelope must be removed")
	}
}

func TestSubmitSettingsRequestRoundTrip(t *testing.T) {
	e := protocolEngine(t)
	SaveProfile(e.Config.ProfilePath, &Profile{Version: 1})
	digest := profileDigest(e.Config.ProfilePath)
	req := SettingsRequest{
		Version: SettingsRequestVersion, Session: e.controller.Session,
		ProfileDigest: digest, Fields: map[string]json.RawMessage{"agent": json.RawMessage(`"claude"`)},
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			e.processSettingsRequests()
			if e.err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	ack, err := submitSettingsRequest(e.Dir, req, 5*time.Second)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	if e.Config.Agent != "claude" {
		t.Fatal("live agent not applied")
	}
}

func TestSubmitTimesOutAsPending(t *testing.T) {
	dir := t.TempDir()
	_, err := submitSettingsRequest(dir, SettingsRequest{Version: SettingsRequestVersion, ID: "pend", Session: "nobody"}, 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("timeout must report pending, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), "pend.json")); statErr != nil {
		t.Fatal("timeout must not remove the request; only the controller claims it")
	}
}

func TestChangedFieldsDiff(t *testing.T) {
	before := settingsFromProfile(&Profile{Version: 1, Agent: "codex", Jobs: 2})
	after := before
	after.Agent = "claude"
	after.Jobs = 4
	fields := changedFields(before, after)
	if len(fields) != 2 {
		t.Fatalf("expected agent and jobs, got %v", fields)
	}
	if string(fields["agent"]) != `"claude"` || string(fields["jobs"]) != "4" {
		t.Fatalf("patch values wrong: %s %s", fields["agent"], fields["jobs"])
	}
	if len(changedFields(before, before)) != 0 {
		t.Fatal("identical settings produced a patch")
	}
}

// The captured operation must be immutable while the owner applies live
// settings. Run under -race.
func TestCaptureIsImmutableUnderLiveChanges(t *testing.T) {
	e := protocolEngine(t)
	e.Config.Model = "m1"
	op := e.capture()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			// A worker goroutine reading only the captured operation.
			_ = op.config.Model + op.agent.Config.Model
		}
	}()
	for i := 0; i < 50; i++ {
		e.applyLiveSettings(&Profile{Version: 1, Model: fmt.Sprintf("m%d", i), Jobs: 2}, map[string]json.RawMessage{"model": json.RawMessage(`"x"`)})
	}
	<-done
	if op.config.Model != "m1" {
		t.Fatal("captured operation mutated")
	}
}

func TestClaimUsesEnvelopeIDForProcessingFile(t *testing.T) {
	e := protocolEngine(t)
	if err := SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Agent: "codex"}); err != nil {
		t.Fatal(err)
	}
	// The file name and the envelope identifier disagree; the claim and the
	// removal must still target the same file.
	req := &SettingsRequest{
		Version: SettingsRequestVersion, ID: "settings-mismatch",
		Session: e.controller.Session, ProfileDigest: profileDigest(e.Config.ProfilePath),
		Fields: map[string]json.RawMessage{"agent": json.RawMessage(`"claude"`)},
	}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "settings-aaa.json"), req); err != nil {
		t.Fatal(err)
	}
	claimed, _, err := claimNextRequest(e.Dir, RequestTypeSettings)
	if err != nil || claimed == nil || claimed.ID != "settings-mismatch" {
		t.Fatalf("claim failed: %v %+v", err, claimed)
	}
	if ack := e.applySettingsRequest(claimed); ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	if err = writeJSON(filepath.Join(ackDir(e.Dir), claimed.ID+".json"), ackFor(e, claimed.ID)); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(processingDir(e.Dir), claimed.ID+".json"))
	entries, _ := os.ReadDir(processingDir(e.Dir))
	if len(entries) != 0 {
		t.Fatalf("stale processing files: %v", entries)
	}
}

func ackFor(e *Engine, id string) SettingsAck {
	return SettingsAck{Version: SettingsRequestVersion, ID: id, Session: e.controller.Session, Status: AckApplied}
}

// Effort and parallel-session requests apply live: the captured operation for
// the next worker sees them, the check semaphore does not change, and the
// outcome names both fields.
func TestEffortAndSessionSettingsApplyLive(t *testing.T) {
	e := protocolEngine(t)
	e.Config.Jobs, e.capacity = 4, 4
	e.Config.Effort = "low"
	e.checkSlots = make(chan struct{}, 2)
	if err := SaveProfile(e.Config.ProfilePath, &Profile{Version: 1, Jobs: 4, Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	digest := profileDigest(e.Config.ProfilePath)
	ack := e.applySettingsRequest(request(e, map[string]any{"effort": "high", "jobs": 2}, digest))
	if ack.Status != AckApplied {
		t.Fatalf("status %s: %s", ack.Status, ack.Message)
	}
	if e.Config.Effort != "high" {
		t.Fatalf("effort must apply live, got %q", e.Config.Effort)
	}
	if e.Config.Jobs != 2 || e.capacity != 2 {
		t.Fatalf("parallel sessions must apply live, got jobs=%d capacity=%d", e.Config.Jobs, e.capacity)
	}
	if cap(e.checkSlots) != 2 {
		t.Fatalf("changing jobs must never change the check semaphore, got %d", cap(e.checkSlots))
	}
	if ack.Outcomes["effort"] != "saved; applied" || ack.Outcomes["jobs"] != "saved; applied at the next dispatch boundary" {
		t.Fatalf("outcomes must name both fields: %+v", ack.Outcomes)
	}
	// The next captured operation sees the applied values.
	op := e.capture()
	if op.config.Effort != "high" || op.config.Jobs != 2 || op.agent.Config.Effort != "high" {
		t.Fatalf("captured operation missed the applied settings: %+v", op.config)
	}
}

// Live effort and session changes during work must never mutate the operation
// a dispatched worker already captured. Run under -race.
func TestCaptureIsImmutableUnderEffortAndSessionChanges(t *testing.T) {
	e := protocolEngine(t)
	e.Config.Jobs, e.capacity = 4, 4
	e.Config.Effort = "low"
	op := e.capture()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			// A worker goroutine reading only the captured operation.
			_ = fmt.Sprintf("%s|%d", op.config.Effort, op.config.Jobs)
			_ = fmt.Sprintf("%s|%d", op.agent.Config.Effort, op.agent.Config.Jobs)
		}
	}()
	for i := 0; i < 50; i++ {
		e.applyLiveSettings(&Profile{Version: 1, Effort: fmt.Sprintf("e%d", i), Jobs: 1 + i%4},
			map[string]json.RawMessage{"effort": json.RawMessage(`"x"`), "jobs": json.RawMessage("2")})
	}
	<-done
	if op.config.Effort != "low" || op.config.Jobs != 4 {
		t.Fatalf("captured operation mutated: effort=%q jobs=%d", op.config.Effort, op.config.Jobs)
	}
	if e.Config.Effort == "low" && e.Config.Jobs == 4 {
		t.Fatal("the live settings never applied")
	}
}

// Idle waiting must wake for a queued controller request instead of sleeping
// out the whole poll interval.
func TestIdleWaitWakesForQueuedRequest(t *testing.T) {
	e := protocolEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	returned := make(chan bool, 1)
	go func() { returned <- e.idleWait(ctx, time.Minute) }()
	select {
	case <-returned:
		t.Fatal("idleWait returned with no request and no deadline")
	case <-time.After(400 * time.Millisecond):
	}
	req := SettingsRequest{Version: SettingsRequestVersion, ID: "settings-wake", Type: RequestTypeSettings, Session: e.controller.Session}
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "settings-wake.json"), req); err != nil {
		t.Fatal(err)
	}
	select {
	case woke := <-returned:
		if !woke {
			t.Fatal("idleWait must report true when a request is waiting")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a queued request must wake idleWait")
	}
	// Cancellation during the wait reports false so the caller stops.
	if err := os.Remove(filepath.Join(requestDir(e.Dir), "settings-wake.json")); err != nil {
		t.Fatal(err)
	}
	go func() { returned <- e.idleWait(ctx, time.Minute) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case woke := <-returned:
		if woke {
			t.Fatal("a cancelled wait must report false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("idleWait ignored cancellation")
	}
}
