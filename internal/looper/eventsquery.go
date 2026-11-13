package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

// The finite event page contract, docs/contracts/schemas/v2/events-page.schema.json.
const (
	eventsPageSchemaVersion = 2
	eventsPageKind          = "looper.events.page"
	// defaultEventPageLimit is the bounded tail a bare finite query returns.
	defaultEventPageLimit = 50
	// maxEventPageLimit is the largest page one finite query may return; a
	// bigger need is an explicit --full export.
	maxEventPageLimit = 500
	// streamEventBatch is the internal page size of an unbounded stream.
	streamEventBatch = 500
)

// eventsPageDocument is the one JSON document a finite event query returns
// under contract version 2. It carries the bounded items, the page metadata
// with its fixed high-water mark, the filters applied, and the continuation.
type eventsPageDocument struct {
	ContractVersion int                `json:"contract_version"`
	SchemaVersion   int                `json:"schema_version"`
	Kind            string             `json:"kind"`
	Items           []eventsPageItem   `json:"items"`
	Page            eventsPageMeta     `json:"page"`
	Filters         *eventsPageFilters `json:"filters,omitempty"`
	Continuation    eventsContinuation `json:"continuation"`
	Store           *eventsStoreRef    `json:"store,omitempty"`
}

// eventsPageItem is one event. The identity fields appear when the recorded
// payload carries them; historical records without them stay readable.
type eventsPageItem struct {
	ID                int64           `json:"id"`
	Time              string          `json:"time"`
	Kind              string          `json:"kind"`
	Message           string          `json:"message"`
	TaskID            string          `json:"task_id,omitempty"`
	AttemptID         string          `json:"attempt_id,omitempty"`
	ControllerSession string          `json:"controller_session,omitempty"`
	Phase             string          `json:"phase,omitempty"`
	RequestID         string          `json:"request_id,omitempty"`
	Data              json.RawMessage `json:"data,omitempty"`
}

type eventsPageMeta struct {
	After       int64  `json:"after"`
	HighWater   int64  `json:"high_water"`
	Returned    int    `json:"returned"`
	Limit       int    `json:"limit,omitempty"`
	Order       string `json:"order"`
	HasMore     bool   `json:"has_more"`
	EmptyReason string `json:"empty_reason,omitempty"`
}

type eventsPageFilters struct {
	Task        string `json:"task,omitempty"`
	Kind        string `json:"kind,omitempty"`
	FullHistory bool   `json:"full_history"`
}

type eventsContinuation struct {
	Cursor  eventsCursorRef `json:"cursor"`
	Command string          `json:"command"`
	HasMore bool            `json:"has_more"`
}

type eventsCursorRef struct {
	EventID    int64  `json:"event_id"`
	Scope      string `json:"scope"`
	StoreID    string `json:"store_id,omitempty"`
	StoreEpoch int64  `json:"store_epoch,omitempty"`
}

type eventsStoreRef struct {
	ID    string `json:"id"`
	Epoch int64  `json:"epoch"`
}

// runEvents serves `looper events` and `looper tail`. A finite query under
// contract version 2 returns one bounded page document; every streaming read
// (--follow, the legacy drain, and the tail command) keeps the
// newline-delimited event stream, one event per line.
func runEvents(ctx context.Context, c Config, store *Store, out io.Writer) error {
	query := eventQueryFor(c)
	// The events command serves the version 2 finite page; the tail command
	// and every followed stream stay newline-delimited.
	if c.Command == "events" && c.Contract == ContractVersionV2 && !c.Follow {
		page, err := store.EventPage(query)
		if err != nil {
			return err
		}
		doc := eventsPageDocumentFor(c, store, query, page)
		if c.JSON {
			return json.NewEncoder(out).Encode(doc)
		}
		renderEventsPage(c, query, page, doc, out)
		return nil
	}
	return streamEvents(ctx, c, store, query, out)
}

// eventQueryFor resolves the configuration into one bounded read. A bare
// version 2 query takes the newest tail; an explicit --after page continues
// forward from it; --full reads the complete history.
func eventQueryFor(c Config) EventQuery {
	q := EventQuery{Task: c.EventTask, Kind: c.EventKind, Full: c.Full}
	switch {
	case c.Full:
		// The store normalizes a full read to a forward read from zero.
	case c.Contract == ContractVersionV2 && !c.AfterSet && !c.Follow && c.Command == "events":
		q.Tail = true
		q.Limit = c.EventLimit
	default:
		q.After = c.After
		q.Limit = c.EventLimit
	}
	return q
}

// eventsPageDocumentFor builds the page document from the read. The empty
// reason distinguishes a filter that matched nothing from a cursor at the
// high-water mark.
func eventsPageDocumentFor(c Config, store *Store, q EventQuery, page *EventsPage) *eventsPageDocument {
	doc := &eventsPageDocument{
		ContractVersion: ContractVersionV2,
		SchemaVersion:   eventsPageSchemaVersion,
		Kind:            eventsPageKind,
		Items:           make([]eventsPageItem, 0, len(page.Items)),
		Page: eventsPageMeta{
			After: page.After, HighWater: page.HighWater,
			Returned: len(page.Items), Order: "ascending", HasMore: page.HasMore,
		},
	}
	if !q.Full {
		doc.Page.Limit = q.Limit
		if doc.Page.Limit <= 0 {
			doc.Page.Limit = defaultEventPageLimit
		}
	}
	if len(page.Items) == 0 {
		if q.Task != "" || q.Kind != "" {
			doc.Page.EmptyReason = "filter_matched_nothing"
		} else {
			doc.Page.EmptyReason = "no_events_after_cursor"
		}
	}
	for _, e := range page.Items {
		doc.Items = append(doc.Items, eventsPageItemFor(e))
	}
	doc.Filters = &eventsPageFilters{Task: q.Task, Kind: q.Kind, FullHistory: q.Full}
	cursor := eventsCursorRef{EventID: page.Cursor, Scope: "store", StoreID: store.identity, StoreEpoch: store.epoch}
	if store.identity != "" {
		doc.Store = &eventsStoreRef{ID: store.identity, Epoch: store.epoch}
	}
	doc.Continuation = eventsContinuation{
		Cursor: cursor, Command: eventContinuationCommand(c, page.Cursor), HasMore: page.HasMore,
	}
	return doc
}

// eventsPageItemFor copies one stored event into its page form, surfacing
// the structured identity the recorded payload carries. Message text stays
// for people and never decides an identity.
func eventsPageItemFor(e Event) eventsPageItem {
	item := eventsPageItem{ID: e.ID, Time: e.Time, Kind: e.Kind, Message: e.Message, Data: e.Data}
	if len(e.Data) > 0 {
		var identity struct {
			TaskID    string `json:"task_id"`
			AttemptID string `json:"attempt_id"`
			Session   string `json:"session"`
			Phase     string `json:"phase"`
			RequestID string `json:"request_id"`
		}
		if json.Unmarshal(e.Data, &identity) == nil {
			item.TaskID, item.AttemptID = identity.TaskID, identity.AttemptID
			item.ControllerSession, item.Phase, item.RequestID = identity.Session, identity.Phase, identity.RequestID
		}
	}
	return item
}

// eventContinuationCommand names the exact next call: the same contract,
// filters, and page size, continuing after the cursor the page advanced to.
func eventContinuationCommand(c Config, cursor int64) string {
	parts := []string{"looper", "events", "--contract", "v2", "--after", strconv.FormatInt(cursor, 10)}
	if c.EventTask != "" {
		parts = append(parts, "--task", c.EventTask)
	}
	if c.EventKind != "" {
		parts = append(parts, "--kind", c.EventKind)
	}
	if c.EventLimit > 0 {
		parts = append(parts, "--limit", strconv.Itoa(c.EventLimit))
	}
	return strings.Join(parts, " ")
}

// olderHistoryRoute names the call that pages the history below a bounded
// tail, preserving the caller's filters and page size.
func olderHistoryRoute(c Config, limit int) string {
	parts := []string{"looper", "events", "--after", "0", "--limit", strconv.Itoa(limit)}
	if c.EventTask != "" {
		parts = append(parts, "--task", c.EventTask)
	}
	if c.EventKind != "" {
		parts = append(parts, "--kind", c.EventKind)
	}
	return strings.Join(parts, " ")
}

// renderEventsPage renders the same page for plain output: the event lines,
// then one route to the history this bounded read did not show.
func renderEventsPage(c Config, q EventQuery, page *EventsPage, doc *eventsPageDocument, out io.Writer) {
	render := NewPresenter(DetectMode(os.Stdin, out, c.Plain), out, func() int { return terminalWidth(fileOf(out)) })
	for _, e := range page.Items {
		if render.Mode == ModeInteractive {
			render.StoredEvent(e)
		} else {
			fmt.Fprintln(out, sanitize(e.Message))
		}
	}
	if render.Mode == ModeInteractive || q.Full {
		return
	}
	route := ""
	switch {
	case page.HasMore:
		route = doc.Continuation.Command
	case q.Tail && len(page.Items) == doc.Page.Limit && len(page.Items) > 0 && page.Items[0].ID > 1:
		// The tail stopped at its bound with older events below it.
		route = olderHistoryRoute(c, doc.Page.Limit)
	}
	if route != "" {
		fmt.Fprintf(out, "Older events exist and are not shown. Read them with: %s\n", route)
	}
}

// streamEvents reads the journal as a newline-delimited stream: the retained
// history first (a bounded tail for the tail command, a drain for events,
// bounded by --limit when given), then new events while --follow is set.
// Filters apply inside every read.
func streamEvents(ctx context.Context, c Config, store *Store, base EventQuery, out io.Writer) error {
	render := NewPresenter(DetectMode(os.Stdin, out, c.Plain), out, func() int { return terminalWidth(fileOf(out)) })
	emit := func(events []Event) error {
		for _, e := range events {
			if c.JSON {
				if err := json.NewEncoder(out).Encode(e); err != nil {
					return err
				}
			} else if render.Mode == ModeInteractive {
				render.StoredEvent(e)
			} else {
				fmt.Fprintln(out, sanitize(e.Message))
			}
		}
		return nil
	}
	// The tail command starts at the newest events; an events query keeps
	// its documented drain from --after (zero without the flag).
	tailFirst := c.Command == "tail" && !c.AfterSet
	after := c.After
	remaining := -1 // the initial read is unbounded without --limit
	if c.EventLimit > 0 {
		remaining = c.EventLimit
	}
	// A bounded plain read that stopped early names the route to the rest.
	route := ""
	noteRoute := func(page *EventsPage, limit int) {
		if c.JSON || render.Mode == ModeInteractive || route != "" {
			return
		}
		switch {
		case len(page.Items) == limit && limit > 0 && page.Items[0].ID > 1 && tailFirst:
			route = olderHistoryRoute(c, limit)
		case page.HasMore && remaining == 0:
			route = eventContinuationCommand(c, page.Cursor)
		}
	}
	drained := false
	for {
		if !drained {
			q := EventQuery{Task: base.Task, Kind: base.Kind, After: after}
			switch {
			case tailFirst:
				q.Tail = true
				q.Limit = remaining
				if q.Limit <= 0 {
					q.Limit = defaultEventPageLimit
				}
			default:
				q.Limit = streamEventBatch
				if remaining > 0 {
					q.Limit = min(streamEventBatch, remaining)
				}
			}
			page, err := store.EventPage(q)
			if err != nil {
				return err
			}
			if err = emit(page.Items); err != nil {
				return err
			}
			if remaining > 0 {
				remaining -= len(page.Items)
			}
			noteRoute(page, q.Limit)
			tailFirst, after = false, page.Cursor
			if len(page.Items) < q.Limit || remaining == 0 {
				drained = true
				if !c.Follow {
					if route != "" {
						fmt.Fprintf(out, "Older events exist and are not shown. Read them with: %s\n", route)
					}
					return nil
				}
				// A followed stream continues from the mark the drain
				// reached. The backlog a bounded drain skipped is history,
				// not new events; it stays behind the caller's --limit.
				after = page.HighWater
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(time.Second):
		}
		page, err := store.EventPage(EventQuery{Task: base.Task, Kind: base.Kind, After: after, Limit: streamEventBatch})
		if err != nil {
			return err
		}
		if err = emit(page.Items); err != nil {
			return err
		}
		after = page.Cursor
	}
}
