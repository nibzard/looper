package looper

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// queueEnvelope writes one claimable settings envelope with a sortable
// identifier so tests can predict the claim and shed order.
func queueEnvelope(t *testing.T, dir, id string) {
	t.Helper()
	if err := writeJSON(filepath.Join(requestDir(dir), id+".json"), SettingsRequest{
		Version: SettingsRequestVersion, ID: id, Type: RequestTypeSettings, Session: "run-test",
	}); err != nil {
		t.Fatal(err)
	}
}

func queueEntries(t *testing.T, dir string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		queueEnvelope(t, dir, fmt.Sprintf("settings-%03d", i))
	}
}

func queuedJSONNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(requestDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	return names
}

// The acceptance core: over one hundred entries in requests/, including
// leftover write temporaries, must not stop claims. Only envelopes count
// toward the pending bound; the sweep holds that bound, so a junk-filled
// directory never wedges the queue again.
func TestClaimIgnoresNonRequestEntries(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(requestDir(dir), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".looper-write-a", ".looper-write-b", "notes.txt"} {
		if err := os.WriteFile(filepath.Join(requestDir(dir), name), []byte("junk"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	queueEntries(t, dir, 100)
	if names := queuedJSONNames(t, dir); len(names) != 100 {
		t.Fatalf("fixture wrote %d envelopes", len(names))
	}
	req, _, err := claimNextRequest(dir, RequestTypeSettings)
	if err != nil {
		t.Fatalf("a junk-filled queue must not refuse claims: %v", err)
	}
	if req == nil || req.ID != "settings-000" {
		t.Fatalf("the oldest envelope must still claim, got %+v", req)
	}
}

// Excess envelopes beyond the bound are shed with an error acknowledgement,
// newest in claim order, while the envelopes the queue keeps still claim.
func TestSweepShedsExcessEnvelopes(t *testing.T) {
	dir := t.TempDir()
	queueEntries(t, dir, 105)
	shed, cleaned, over := sweepRequestQueue(dir, "run-test")
	if len(shed) != 5 || cleaned != 0 || over != 0 {
		t.Fatalf("sweep shed %d, cleaned %d, over %d; want 5, 0, 0", len(shed), cleaned, over)
	}
	names := queuedJSONNames(t, dir)
	if len(names) != 100 {
		t.Fatalf("the queue must hold the bound after the sweep, got %d", len(names))
	}
	for _, id := range []string{"settings-100", "settings-101", "settings-102", "settings-103", "settings-104"} {
		if _, statErr := os.Stat(filepath.Join(requestDir(dir), id+".json")); statErr == nil {
			t.Fatalf("excess envelope %s must be removed", id)
		}
		ack, ok := readRequestAck(dir, id)
		if !ok {
			t.Fatalf("shed envelope %s received no acknowledgement", id)
		}
		if ack.Status != AckError {
			t.Fatalf("shed envelope %s ack status %q", id, ack.Status)
		}
		if !strings.Contains(ack.Message, "Resubmit") {
			t.Fatalf("shed envelope %s ack %q must tell the submitter to resubmit", id, ack.Message)
		}
	}
	req, _, err := claimNextRequest(dir, RequestTypeSettings)
	if err != nil || req == nil || req.ID != "settings-000" {
		t.Fatalf("the oldest envelope must still claim after the sweep: %+v, %v", req, err)
	}
}

// One pass sheds at most the bound, so a flooded boundary stays responsive;
// over reports the excess that continues at the next boundaries.
func TestSweepBoundsSheddingPerPass(t *testing.T) {
	dir := t.TempDir()
	queueEntries(t, dir, 250)
	shed, _, over := sweepRequestQueue(dir, "run-test")
	if len(shed) != 100 || over != 50 {
		t.Fatalf("first pass shed %d, over %d; want 100, 50", len(shed), over)
	}
	if names := queuedJSONNames(t, dir); len(names) != 150 {
		t.Fatalf("the queue must hold 150 after a capped pass, got %d", len(names))
	}
	shed, _, over = sweepRequestQueue(dir, "run-test")
	if len(shed) != 50 || over != 0 {
		t.Fatalf("second pass shed %d, over %d; want 50, 0", len(shed), over)
	}
	if names := queuedJSONNames(t, dir); len(names) != 100 {
		t.Fatalf("the queue must hold the bound after recovery, got %d", len(names))
	}
}

// Stale non-request entries are removed; young ones may belong to a live
// writer and stay for a later pass.
func TestSweepRemovesStaleNonRequestEntries(t *testing.T) {
	dir := t.TempDir()
	queueEntries(t, dir, 2)
	stale := time.Now().Add(-2 * staleRequestEntryAge)
	for _, name := range []string{".looper-write-stale", "stray.txt"} {
		path := filepath.Join(requestDir(dir), name)
		if err := os.WriteFile(path, []byte("junk"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(requestDir(dir), ".looper-write-live"), []byte("staged"), 0600); err != nil {
		t.Fatal(err)
	}
	shed, cleaned, over := sweepRequestQueue(dir, "run-test")
	if len(shed) != 0 || cleaned != 2 || over != 0 {
		t.Fatalf("sweep shed %d, cleaned %d, over %d; want 0, 2, 0", len(shed), cleaned, over)
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(dir), ".looper-write-live")); statErr != nil {
		t.Fatal("a young write temporary may still belong to a live writer")
	}
	if names := queuedJSONNames(t, dir); len(names) != 2 {
		t.Fatalf("envelopes under the bound must be untouched, got %d", len(names))
	}
}

// An envelope whose acknowledgement cannot be written stays put and is
// retried, never silently deleted.
func TestSweepKeepsEnvelopeWhenAckWriteFails(t *testing.T) {
	dir := t.TempDir()
	queueEntries(t, dir, 101)
	if err := os.WriteFile(filepath.Join(dir, "acks"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	shed, _, over := sweepRequestQueue(dir, "run-test")
	if len(shed) != 0 || over != 1 {
		t.Fatalf("sweep shed %d, over %d; want 0, 1", len(shed), over)
	}
	if names := queuedJSONNames(t, dir); len(names) != 101 {
		t.Fatalf("every envelope must stay while its acknowledgement cannot be written, got %d", len(names))
	}
}

// A pass whose claims fail must not let queued envelopes wake the idle wait:
// the wait sleeps out the poll interval, the next boundary retries, and the
// wake returns once claims answer again.
func TestIdleWaitSleepsWhenClaimsFail(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{}
	queueEnvelope(t, e.Dir, "settings-1")
	// A file where processing/ belongs makes every claim fail while the
	// envelope stays readable, so requestsWaiting keeps reporting true.
	if err := os.WriteFile(filepath.Join(e.Dir, "processing"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	e.processSettingsRequests()
	if !e.suppressRequestWake {
		t.Fatal("a claim error must suppress this pass's request wake")
	}
	if _, ok := readRequestAck(e.Dir, "settings-1"); ok {
		t.Fatal("a failed claim must not answer the envelope")
	}
	start := time.Now()
	if !e.idleWait(context.Background(), 300*time.Millisecond) {
		t.Fatal("idle wait reported cancellation")
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("idle wait woke after %s on envelopes the boundary failed to claim", elapsed)
	}

	// The next boundary clears the mark, retries, and answers the envelope.
	if err := os.Remove(filepath.Join(e.Dir, "processing")); err != nil {
		t.Fatal(err)
	}
	if err := e.processControllerRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.suppressRequestWake {
		t.Fatal("a healthy boundary must clear the request wake suppression")
	}
	if _, ok := readRequestAck(e.Dir, "settings-1"); !ok {
		t.Fatal("the retried boundary must answer the queued envelope")
	}

	// With claims healthy again, a queued envelope wakes the wait at once.
	queueEnvelope(t, e.Dir, "settings-2")
	start = time.Now()
	if !e.idleWait(context.Background(), 30*time.Second) {
		t.Fatal("idle wait reported cancellation")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("idle wait slept %s despite a claimable envelope", elapsed)
	}
}

// The loop boundary recovers a flooded, junk-filled queue end to end: excess
// envelopes get error acknowledgements, stale junk is removed, an invalid
// envelope is answered instead of left as a dead letter, and the oldest
// valid envelope is claimed.
func TestProcessControllerRequestsRecoversFloodedQueue(t *testing.T) {
	e := protocolEngine(t)
	e.State = &State{}
	queueEntries(t, e.Dir, 129)
	// An invalid envelope that sorts first survives the shed and must be
	// answered by the claim path, not left as a dead letter.
	if err := writeJSON(filepath.Join(requestDir(e.Dir), "a-invalid.json"), SettingsRequest{
		Version: 0, ID: "a-invalid", Type: RequestTypeSettings, Session: "run-test",
	}); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * staleRequestEntryAge)
	for _, name := range []string{".looper-write-stale", "stray.bin"} {
		path := filepath.Join(requestDir(e.Dir), name)
		if err := os.WriteFile(path, []byte("junk"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.processControllerRequests(context.Background()); err != nil {
		t.Fatal(err)
	}
	var recovered *Event
	for i, ev := range storedEvents(t, e) {
		if ev.Kind == "request-queue" {
			recovered = &storedEvents(t, e)[i]
		}
	}
	if recovered == nil {
		t.Fatal("the boundary must journal the queue recovery")
	}
	for _, want := range []string{"30 envelope(s)", "100 pending limit", "2 stale non-request entries"} {
		if !strings.Contains(recovered.Message, want) {
			t.Fatalf("recovery event %q does not contain %q", recovered.Message, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(e.Dir), ".looper-write-stale")); statErr == nil {
		t.Fatal("stale junk must be removed")
	}
	if ack, ok := readRequestAck(e.Dir, "a-invalid"); !ok || ack.Status != AckError {
		t.Fatalf("the invalid envelope must receive an error acknowledgement, got %+v, %v", ack, ok)
	}
	if _, statErr := os.Stat(filepath.Join(requestDir(e.Dir), "a-invalid.json")); statErr == nil {
		t.Fatal("the invalid envelope must be removed")
	}
	// The oldest valid envelope was claimed and answered; the whole kept
	// bound drains through the claim loop, and a shed envelope carries the
	// overflow answer rather than a claim result.
	ack, ok := readRequestAck(e.Dir, "settings-000")
	if !ok {
		t.Fatal("the oldest envelope must be answered")
	}
	if strings.Contains(ack.Message, "Resubmit") {
		t.Fatalf("a kept envelope must be claimed, not shed: %+v", ack)
	}
	ack, ok = readRequestAck(e.Dir, "settings-128")
	if !ok || !strings.Contains(ack.Message, "Resubmit") {
		t.Fatalf("the newest envelope must carry the overflow answer, got %+v, %v", ack, ok)
	}
	if names := queuedJSONNames(t, e.Dir); len(names) != 0 {
		t.Fatalf("a recovered queue must drain through claims, %d envelope(s) remain", len(names))
	}
}
