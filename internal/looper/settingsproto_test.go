package looper

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
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
	writeJSON(filepath.Join(requestDir(dir), "old-settings.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "a", Type: RequestTypeSettings, Session: "old"})
	writeJSON(filepath.Join(requestDir(dir), "keep-import.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "b", Type: RequestTypePlanImport, Session: "old"})
	writeJSON(filepath.Join(processingDir(dir), "stuck.json"), SettingsRequest{Version: SettingsRequestVersion, ID: "c", Type: RequestTypeSettings, Session: "old"})
	writeJSON(filepath.Join(ackDir(dir), "old-ack.json"), SettingsAck{Version: 1, ID: "x"})
	clearSessionFiles(dir)
	if _, err := os.Stat(filepath.Join(requestDir(dir), "keep-import.json")); err != nil {
		t.Fatal("plan-import request must survive session cleanup")
	}
	if _, err := os.Stat(filepath.Join(requestDir(dir), "old-settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings request from an old session kept")
	}
	if _, err := os.Stat(filepath.Join(processingDir(dir), "stuck.json")); !os.IsNotExist(err) {
		t.Fatal("stuck processing request kept")
	}
	if _, err := os.Stat(filepath.Join(ackDir(dir), "old-ack.json")); !os.IsNotExist(err) {
		t.Fatal("old acknowledgement kept")
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
