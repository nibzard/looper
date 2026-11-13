package looper

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// seedEventStore writes count events where every third carries the task
// identity T015 in its recorded payload and every fifth is a check for T018.
// The seeding shares one transaction, so a fixture of several hundred
// events costs one durable commit instead of one per event.
func seedEventStore(t *testing.T, path string, count int) *Store {
	t.Helper()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	seedEvents(t, s, count, func(i int) (string, string, any) {
		kind, data := "progress", any(nil)
		switch {
		case i%3 == 0:
			kind, data = "worker_activity", map[string]string{"task_id": "T015", "attempt_id": "task-abc", "session": "run-1", "phase": "running"}
		case i%5 == 0:
			kind, data = "check", map[string]string{"task_id": "T018"}
		}
		return kind, fmt.Sprintf("event %d", i+1), data
	})
	return s
}

// eventsTestConfig builds the configuration the events command reads and the
// runtime directory its store lives in.
func eventsTestConfig(t *testing.T, command string) (Config, string) {
	t.Helper()
	c := Config{Root: t.TempDir(), BaseDir: t.TempDir(), Todo: "to-do.json", Command: command, JSON: true, Contract: ContractVersionV2, Agent: "claude"}
	_, dir := projectPaths(c)
	return c, dir
}

// runEventsCommand drives the production read path and decodes the page
// document it emits.
func runEventsCommand(t *testing.T, c Config) *eventsPageDocument {
	t.Helper()
	var out strings.Builder
	store, err := OpenStoreReadonly(filepath.Join(mustDir(t, c), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = runEvents(context.Background(), c, store, &out); err != nil {
		t.Fatalf("runEvents: %v", err)
	}
	var doc eventsPageDocument
	if err = json.Unmarshal([]byte(out.String()), &doc); err != nil {
		t.Fatalf("decode page document: %v\n%s", err, out.String())
	}
	return &doc
}

// mustDir recomputes the runtime directory for the configuration.
func mustDir(t *testing.T, c Config) string {
	t.Helper()
	_, dir := projectPaths(c)
	return dir
}

// TestEventsPageDefaultTailBoundedAndChronological covers the bare finite
// query: the newest default of fifty events, oldest first within that tail,
// never more than the page limit on a large journal.
func TestEventsPageDefaultTailBoundedAndChronological(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	seedEventStore(t, filepath.Join(dir, "state.db"), 120)
	c.Command = "events"

	doc := runEventsCommand(t, c)
	if len(doc.Items) != 50 || doc.Page.Returned != 50 {
		t.Fatalf("bare query returned %d items, want the default tail of 50", len(doc.Items))
	}
	if doc.Page.Limit != 50 || doc.Page.After != 0 || doc.Page.Order != "ascending" {
		t.Fatalf("page metadata wrong: %+v", doc.Page)
	}
	ids := make([]int64, 0, 50)
	for _, item := range doc.Items {
		ids = append(ids, item.ID)
	}
	if ids[0] != 71 || ids[49] != 120 {
		t.Fatalf("tail window wrong: first %d last %d", ids[0], ids[49])
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] <= ids[i-1] {
			t.Fatalf("tail is not chronological at %d: %v", i, ids)
		}
	}
	if doc.Page.HasMore || doc.Continuation.Cursor.EventID != 120 {
		t.Fatalf("tail continuation wrong: %+v", doc.Continuation)
	}
	// A configurable bound shrinks the page and stays chronological.
	cc := c
	cc.EventLimit = 7
	doc = runEventsCommand(t, cc)
	if len(doc.Items) != 7 || doc.Page.Limit != 7 || doc.Items[0].ID != 114 || doc.Items[6].ID != 120 {
		t.Fatalf("limit 7 tail wrong: %d items first %d", len(doc.Items), doc.Items[0].ID)
	}
}

// TestEventsPageAfterIsExclusive covers the documented exclusive-after bound:
// a page strictly after N returns N+1 onward, and after at the high-water
// mark returns nothing with a continuable cursor.
func TestEventsPageAfterIsExclusive(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	seedEventStore(t, filepath.Join(dir, "state.db"), 30)
	cc := c
	cc.After, cc.AfterSet = 28, true
	doc := runEventsCommand(t, cc)
	if len(doc.Items) != 2 || doc.Items[0].ID != 29 || doc.Items[1].ID != 30 {
		t.Fatalf("exclusive-after wrong: %+v", doc.Items)
	}
	cc.After = 30
	doc = runEventsCommand(t, cc)
	if len(doc.Items) != 0 || doc.Page.EmptyReason != "no_events_after_cursor" {
		t.Fatalf("cursor at high-water must be empty: %+v", doc.Page)
	}
	if doc.Continuation.Cursor.EventID != 30 {
		t.Fatalf("empty page must keep the cursor continuable: %+v", doc.Continuation)
	}
	// A position beyond the retained store is a reset, never a silent page.
	cc.After = 99
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.EventPage(eventQueryFor(cc))
	var invalid *cursorInvalidError
	if !errors.As(err, &invalid) || invalid.Code() != "cursor_invalid" {
		t.Fatalf("beyond-store position must be a cursor error: %v", err)
	}
}

// TestEventsFilteredContinuationHasNoGaps walks a filtered journal through
// its continuation cursors and requires every matching event exactly once,
// including across an empty page that advances the cursor.
func TestEventsFilteredContinuationHasNoGaps(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	w := seedEventStore(t, filepath.Join(dir, "state.db"), 60)
	// The complete matching set, read directly from the store.
	var truth []int64
	rows, err := w.db.Query("SELECT id FROM events WHERE json_extract(data,'$.task_id')='T015' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		truth = append(truth, id)
	}
	rows.Close()
	if len(truth) == 0 {
		t.Fatal("scenario matched no task events")
	}

	var walked []int64
	after, limit := int64(0), 3
	cc := c
	cc.EventTask, cc.EventLimit = "T015", limit
	for {
		cc.After, cc.AfterSet = after, true
		doc := runEventsCommand(t, cc)
		if len(doc.Items) > limit {
			t.Fatalf("page exceeded its limit: %d", len(doc.Items))
		}
		for _, item := range doc.Items {
			if item.TaskID != "T015" {
				t.Fatalf("filter leaked event %d of task %q", item.ID, item.TaskID)
			}
			walked = append(walked, item.ID)
		}
		if after >= doc.Page.HighWater && doc.Continuation.Cursor.EventID < doc.Page.HighWater {
			t.Fatalf("empty filtered page stalled at %d below high-water %d", doc.Continuation.Cursor.EventID, doc.Page.HighWater)
		}
		after = doc.Continuation.Cursor.EventID
		if !doc.Continuation.HasMore && after >= doc.Page.HighWater {
			break
		}
	}
	if fmt.Sprint(walked) != fmt.Sprint(truth) {
		t.Fatalf("filtered walk has gaps or repeats:\n walked %v\n truth  %v", walked, truth)
	}

	// A filtered query whose filter matches nothing still advances: the
	// cursor reports the fixed high-water mark, so the next matching event
	// after the page arrives without rescanning or stalling.
	cc.EventTask = "T999"
	cc.After, cc.AfterSet = 0, true
	doc := runEventsCommand(t, cc)
	if len(doc.Items) != 0 || doc.Page.EmptyReason != "filter_matched_nothing" {
		t.Fatalf("empty filtered page wrong: %+v", doc.Page)
	}
	high := doc.Page.HighWater
	if doc.Continuation.Cursor.EventID != high {
		t.Fatalf("empty filtered page must advance to %d, got %d", high, doc.Continuation.Cursor.EventID)
	}
	if _, err = w.Event("worker_activity", "late match", map[string]string{"task_id": "T999", "phase": "running"}); err != nil {
		t.Fatal(err)
	}
	cc.After = high
	doc = runEventsCommand(t, cc)
	if len(doc.Items) != 1 || doc.Items[0].TaskID != "T999" {
		t.Fatalf("advanced cursor missed the new matching event: %+v", doc.Items)
	}
}

// TestEventsPageCompletesUnderConcurrentWriters covers the fixed high-water
// mark: while a writer commits continuously, every finite page returns at
// most its limit, ends at a mark the writer had already reached, and the
// walk keeps completing. The writer runs until the walk has observed both,
// then stops so the drain to the final mark no longer races the writer's
// commit throughput; the deadlines below are failure bounds against a page
// that never completes, not budgets the healthy path must outrun.
func TestEventsPageCompletesUnderConcurrentWriters(t *testing.T) {
	_, dir := eventsTestConfig(t, "events")
	const seeded = 40
	w := seedEventStore(t, filepath.Join(dir, "state.db"), seeded)
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// readPage walks one finite page and checks the two invariants that
	// hold under a concurrent writer: the page bound and the fixed mark.
	readPage := func(after int64) (*EventsPage, int64) {
		t.Helper()
		page, err := store.EventPage(EventQuery{After: after, Limit: 25})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Items) > 25 {
			t.Fatalf("page returned %d items over its limit", len(page.Items))
		}
		for _, e := range page.Items {
			if e.ID > page.HighWater {
				t.Fatalf("event %d beyond the fixed mark %d", e.ID, page.HighWater)
			}
		}
		return page, page.Cursor
	}

	// The writer commits until the walk signals, with a backstop count so
	// a broken walk cannot keep it committing forever.
	stop := make(chan struct{})
	done := make(chan struct{})
	var writeErr error
	go func() {
		defer close(done)
		for i := 0; i < 500; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := w.Event("chatter", fmt.Sprintf("c%d", i), nil); err != nil {
				writeErr = err
				return
			}
		}
	}()

	// Phase one: pages complete while the writer commits. The walk must
	// see the fixed mark advance past the forty seeded events (commits
	// really landed mid-walk) and complete at least two non-empty pages.
	bound := time.Now().Add(testBudget(30 * time.Second))
	after, pages, nonEmpty := int64(0), 0, 0
	advanced, writerDone := false, false
	for {
		var page *EventsPage
		page, after = readPage(after)
		pages++
		if len(page.Items) > 0 {
			nonEmpty++
		}
		if page.HighWater > seeded {
			advanced = true
		}
		select {
		case <-done:
			writerDone = true
		default:
		}
		if (advanced && nonEmpty >= 2) || writerDone {
			break
		}
		if time.Now().After(bound) {
			t.Fatal("pages did not complete while the writer committed")
		}
	}
	close(stop)
	<-done
	if writeErr != nil {
		t.Fatalf("concurrent writer failed: %v", writeErr)
	}

	// Phase two: drain to the final mark with the writer stopped, so
	// completion depends only on the page reads.
	bound = time.Now().Add(testBudget(30 * time.Second))
	for {
		var page *EventsPage
		page, after = readPage(after)
		pages++
		if !page.HasMore && after >= page.HighWater {
			break
		}
		if time.Now().After(bound) {
			t.Fatal("finite pages did not complete under concurrent writers")
		}
	}
	if after <= seeded {
		t.Fatalf("walk ended at %d; the writer's commits never became pages", after)
	}
	if pages < 2 {
		t.Fatalf("only %d pages read; the race was not exercised", pages)
	}
}

// TestEventsPageDocumentMatchesSchema runs the production finite query and
// validates its document against the published version 2 schema: a bounded
// tail, an empty filtered page, and a full-history export.
func TestEventsPageDocumentMatchesSchema(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	seedEventStore(t, filepath.Join(dir, "state.db"), 80)
	schema, err := os.ReadFile(filepath.Join(legacyContractDocs, "schemas", "v2", "events-page.schema.json"))
	if err != nil {
		t.Skipf("events-page schema is absent: %v", err)
	}
	validate := func(doc *eventsPageDocument) {
		t.Helper()
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err = contractschema.ValidateJSON(schema, data); err != nil {
			t.Fatalf("produced page violates its schema: %v\n%s", err, data)
		}
	}
	validate(runEventsCommand(t, c))

	cc := c
	cc.EventTask = "T-nope"
	validate(runEventsCommand(t, cc))

	cc = c
	cc.Full = true
	doc := runEventsCommand(t, cc)
	if doc.Page.Returned != 80 || doc.Filters.FullHistory != true || doc.Page.Limit != 0 {
		t.Fatalf("full export wrong: %+v", doc.Page)
	}
	validate(doc)
}

// TestEventsLegacyDrainKeepsLineShapeAndBoundedFlags pins the legacy
// contract: without the new flags the drain keeps its exact newline-delimited
// lines, and the new flags bound or filter the same lines when asked for.
func TestEventsLegacyDrainKeepsLineShapeAndBoundedFlags(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	seedEventStore(t, filepath.Join(dir, "state.db"), 12)
	c.Contract = ContractVersionLegacy

	run := func(c Config) []string {
		t.Helper()
		var out strings.Builder
		store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if err = runEvents(context.Background(), c, store, &out); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if lines[0] == "" {
			return nil
		}
		return lines
	}
	drain := run(c)
	if len(drain) != 12 {
		t.Fatalf("legacy drain returned %d lines, want the whole journal", len(drain))
	}
	var first Event
	if err := json.Unmarshal([]byte(drain[0]), &first); err != nil {
		t.Fatalf("legacy line is not an event document: %v", err)
	}
	if first.ID != 1 || first.Message != "event 1" {
		t.Fatalf("legacy drain order changed: %+v", first)
	}

	bounded := c
	bounded.EventLimit = 4
	if lines := run(bounded); len(lines) != 4 {
		t.Fatalf("--limit bounded the legacy drain to %d lines", len(lines))
	}
	filtered := c
	filtered.EventTask = "T015"
	lines := run(filtered)
	if len(lines) != 4 { // events 1,4,7,10 carry T015
		t.Fatalf("--task filtered the legacy drain to %d lines, want 4", len(lines))
	}
	for _, line := range lines {
		var e Event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		var payload struct {
			TaskID string `json:"task_id"`
		}
		if err := json.Unmarshal(e.Data, &payload); err != nil || payload.TaskID != "T015" {
			t.Fatalf("filter leaked event %d", e.ID)
		}
	}
}

// TestTailCommandReadsNewestEvents covers the plain tail command: it reads
// the newest bounded tail and exits without --follow.
func TestTailCommandReadsNewestEvents(t *testing.T) {
	c, dir := eventsTestConfig(t, "tail")
	seedEventStore(t, filepath.Join(dir, "state.db"), 70)
	c.JSON = false
	var out strings.Builder
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err = runEvents(context.Background(), c, store, &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	// Fifty event lines and one route to the older history this tail cut off.
	if len(lines) != 51 {
		t.Fatalf("tail printed %d lines, want the 50 newest plus the older-history route", len(lines))
	}
	if !strings.Contains(lines[49], "event 70") || !strings.Contains(lines[0], "event 21") {
		t.Fatalf("tail window wrong: first %q last %q", lines[0], lines[49])
	}
	if !strings.Contains(lines[50], "Older events exist") || !strings.Contains(lines[50], "looper events --after 0") {
		t.Fatalf("older-history route missing: %q", lines[50])
	}
}

// TestEventQueryFlagValidation covers the refused combinations: a page over
// its maximum, contradictions around the complete export, and event options
// on commands that do not read the journal.
func TestEventQueryFlagValidation(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"limit over maximum", []string{"events", "--limit", "501"}},
		{"limit not a number", []string{"events", "--limit", "many"}},
		{"full with after", []string{"events", "--full", "--after", "4"}},
		{"full with follow", []string{"events", "--full", "--follow"}},
		{"full with limit", []string{"events", "--full", "--limit", "10"}},
		{"full on tail", []string{"tail", "--full"}},
		{"task on status", []string{"status", "--task", "T1"}},
		{"kind on doctor", []string{"doctor", "--kind", "check"}},
	}
	for _, tc := range cases {
		_, err := Parse(tc.args)
		if err == nil {
			t.Errorf("%s: parsed without error", tc.name)
			continue
		}
		var p *PublicError
		if !errors.As(err, &p) || p.ExitCode != exitUsage {
			t.Errorf("%s: not a usage failure: %v", tc.name, err)
		}
	}
	// The supported combinations parse.
	for _, args := range [][]string{
		{"events", "--limit", "500"},
		{"events", "--task", "T015", "--kind", "check"},
		{"events", "--full"},
		{"events", "--follow", "--limit", "10"},
		{"tail", "--limit", "5"},
	} {
		if _, err := Parse(args); err != nil {
			t.Errorf("%v must parse: %v", args, err)
		}
	}
}

// TestEventsCommandThroughMain pins the public entry point: the version 2
// finite query prints one page document and exits zero, and a usage failure
// exits two without reading any store.
func TestEventsCommandThroughMain(t *testing.T) {
	root, base := t.TempDir(), t.TempDir()
	c := Config{Root: root, Todo: "to-do.json", BaseDir: base}
	_, dir := projectPaths(c)
	seedEventStore(t, filepath.Join(dir, "state.db"), 5)
	t.Setenv("LOOPER_BASE_DIR", base)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(previous) })

	var out strings.Builder
	code := Main([]string{"events", "--contract", "v2", "--json", "--limit", "2"}, &out, &out)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	var doc eventsPageDocument
	if err = json.Unmarshal([]byte(strings.TrimSpace(out.String())), &doc); err != nil {
		t.Fatalf("stdout is not one page document: %v\n%s", err, out.String())
	}
	if doc.Kind != eventsPageKind || len(doc.Items) != 2 {
		t.Fatalf("page wrong through main: %s", out.String())
	}

	out.Reset()
	code = Main([]string{"events", "--limit", "0"}, &out, &out)
	if code != exitUsage {
		t.Fatalf("usage failure exited %d", code)
	}
}

// lockedBuilder is a strings.Builder safe to poll while a stream writes: the
// follow goroutine encodes into it while the test reads progress.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestEventsFollowStreamsOnlyNewEventsAfterBoundedDrain covers a followed
// stream with --limit: the initial history read stays bounded, the backlog
// it skipped is not replayed as new, and an event committed after the drain
// arrives on the stream.
func TestEventsFollowStreamsOnlyNewEventsAfterBoundedDrain(t *testing.T) {
	c, dir := eventsTestConfig(t, "events")
	w := seedEventStore(t, filepath.Join(dir, "state.db"), 20)
	c.Contract = ContractVersionLegacy
	c.Follow = true
	c.EventLimit = 3

	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithCancel(context.Background())
	var out lockedBuilder
	streamed := make(chan error, 1)
	go func() {
		streamed <- streamEvents(ctx, c, store, eventQueryFor(c), &out)
	}()
	// The drain is synchronous and bounded; the backlog beyond it never
	// streams, so wait past one poll interval and commit one new event.
	time.Sleep(1500 * time.Millisecond)
	if _, err = w.Event("progress", "fresh after drain", nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(testBudget(5 * time.Second))
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), "fresh after drain") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	if err = <-streamed; err != nil {
		t.Fatalf("stream: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("stream delivered %d lines, want the 3 bounded history lines and the one new event:\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[2], "event 3") || !strings.Contains(lines[3], "fresh after drain") {
		t.Fatalf("stream order wrong:\n%s", out.String())
	}
}

// TestEventsPageOnStoreWithoutIndexes covers a read-only attach to a store
// an older controller created: the journal still pages, filters, and tails
// without any migration of that store.
func TestEventsPageOnStoreWithoutIndexes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE events (id INTEGER PRIMARY KEY, time TEXT NOT NULL, kind TEXT NOT NULL, message TEXT NOT NULL, data TEXT NOT NULL);
INSERT INTO events(time,kind,message,data) VALUES ('2026-11-05T10:00:00Z','worker_activity','old one','{"task_id":"T1"}'),('2026-11-05T10:00:01Z','progress','old two','null')`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenStoreReadonly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var page *EventsPage
	page, err = store.EventPage(EventQuery{Tail: true, Limit: 10})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != 1 {
		t.Fatalf("legacy store tail broken: %v %+v", err, page)
	}
	filtered, err := store.EventPage(EventQuery{Task: "T1"})
	if err != nil || len(filtered.Items) != 1 || filtered.Items[0].ID != 1 {
		t.Fatalf("legacy store filter broken: %v %+v", err, filtered)
	}
	if filtered.Cursor != 2 {
		t.Fatalf("legacy store continuation must advance to the mark: %d", filtered.Cursor)
	}
}
