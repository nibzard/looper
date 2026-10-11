package ui

// ViewVersion is the presentation-snapshot contract version.
const ViewVersion = 1

// ViewSnapshot is the data contract for dashboards. The owner loop creates
// copies; the UI never reads mutable engine objects. Events carry messages,
// not complete state, so the snapshot holds the authoritative presentation
// state and the journal stays authoritative for event history.
type ViewSnapshot struct {
	Version         int           `json:"version"`
	Session         string        `json:"session"`
	Mode            string        `json:"mode"`
	Started         string        `json:"started,omitempty"`
	Now             string        `json:"now,omitempty"`
	State           string        `json:"state,omitempty"`
	ActivityDetail  string        `json:"activity_detail,omitempty"`
	ActivityUpdated string        `json:"activity_updated,omitempty"`
	ActivityLog     string        `json:"activity_log,omitempty"`
	Goal            string        `json:"goal,omitempty"`
	Counters        ViewCounters  `json:"counters"`
	Attempts        []AttemptView `json:"attempts"`
	Ready           []TaskView    `json:"ready"`
	Blocked         []BlockedView `json:"blocked"`
	EventCursor     int64         `json:"event_cursor"`
	Draining        bool          `json:"draining,omitempty"`
	Finished        bool          `json:"finished,omitempty"`
	Stale           bool          `json:"stale,omitempty"`
}

// ViewCounters carries board counters. Missing timing fields render as
// unknown rather than being invented.
type ViewCounters struct {
	Done       int `json:"done"`
	Total      int `json:"total"`
	Ready      int `json:"ready"`
	Blocked    int `json:"blocked"`
	InFlight   int `json:"in_flight"`
	Checking   int `json:"checking"`
	Integrated int `json:"integrated"`
}

// AttemptView describes one active or retained attempt.
type AttemptView struct {
	ID       string `json:"id"`
	TaskID   string `json:"task_id"`
	Title    string `json:"title"`
	Phase    string `json:"phase"`
	Agent    string `json:"agent,omitempty"`
	Repairs  int    `json:"repairs"`
	Base     string `json:"base,omitempty"`
	Worktree string `json:"worktree,omitempty"`
}

// TaskView describes a ready task.
type TaskView struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// BlockedView describes an explicitly blocked task with its reasons.
type BlockedView struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Reasons []string `json:"reasons"`
}

// JournalEvent is one journaled event for the bounded tail. Observers page
// events by identifier.
type JournalEvent struct {
	ID      int64  `json:"id"`
	Time    string `json:"time"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// PhaseLabel renders an attempt phase for display.
func PhaseLabel(phase string) string {
	switch phase {
	case "running":
		return "implementing"
	case "verifying":
		return "checking"
	case "ready":
		return "queued"
	case "resume":
		return "resuming"
	case "done":
		return "done"
	case "blocked":
		return "blocked"
	case "integrated":
		return "integrating"
	default:
		return phase
	}
}
