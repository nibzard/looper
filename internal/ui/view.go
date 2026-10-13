package ui

// ViewVersion is the presentation-snapshot contract version.
const ViewVersion = 1

// ViewSnapshot is the data contract for dashboards. The owner loop creates
// copies; the UI never reads mutable engine objects. Events carry messages,
// not complete state, so the snapshot holds the authoritative presentation
// state and the journal stays authoritative for event history.
type ViewSnapshot struct {
	Version         int            `json:"version"`
	Session         string         `json:"session"`
	Mode            string         `json:"mode"`
	Started         string         `json:"started,omitempty"`
	Now             string         `json:"now,omitempty"`
	State           string         `json:"state,omitempty"`
	ActivityAgent   string         `json:"activity_agent,omitempty"`
	ActivityDetail  string         `json:"activity_detail,omitempty"`
	ActivityUpdated string         `json:"activity_updated,omitempty"`
	ActivityLog     string         `json:"activity_log,omitempty"`
	Goal            string         `json:"goal,omitempty"`
	Project         string         `json:"project,omitempty"`
	Tasks           []TaskView     `json:"tasks,omitempty"`
	WorkerLimit     int            `json:"worker_limit,omitempty"`
	CheckLimit      int            `json:"check_limit,omitempty"`
	Pace            *PaceView      `json:"pace,omitempty"`
	RecentOutcomes  []JournalEvent `json:"recent_outcomes,omitempty"`
	Counters        ViewCounters   `json:"counters"`
	Attempts        []AttemptView  `json:"attempts"`
	Ready           []TaskView     `json:"ready"`
	Blocked         []BlockedView  `json:"blocked"`
	EventCursor     int64          `json:"event_cursor"`
	Draining        bool           `json:"draining,omitempty"`
	Finished        bool           `json:"finished,omitempty"`
	Stale           bool           `json:"stale,omitempty"`
	// Bootstrapping marks the phase before the controller publishes its
	// first view. The observer synthesizes the snapshot and fills the
	// bootstrap evidence below; ActivityLog names the agent event stream.
	Bootstrapping        bool    `json:"bootstrapping,omitempty"`
	BootstrapBytes       int64   `json:"bootstrap_bytes,omitempty"`
	BootstrapIdleSeconds float64 `json:"bootstrap_idle_seconds,omitempty"`
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
	ID             string `json:"id"`
	TaskID         string `json:"task_id"`
	Title          string `json:"title"`
	Phase          string `json:"phase"`
	Agent          string `json:"agent,omitempty"`
	Repairs        int    `json:"repairs"`
	Base           string `json:"base,omitempty"`
	Worktree       string `json:"worktree,omitempty"`
	LogDir         string `json:"log_dir,omitempty"`
	LastOutput     string `json:"last_output,omitempty"`
	ProcessState   string `json:"process_state,omitempty"`
	ProcessStarted string `json:"process_started,omitempty"`
	Repeats        int    `json:"repeats,omitempty"`
}

// TaskView describes a ready task.
type TaskView struct {
	ID          string       `json:"id"`
	Title       string       `json:"title"`
	Status      string       `json:"status,omitempty"`
	Revision    string       `json:"revision,omitempty"`
	Details     string       `json:"details,omitempty"`
	DependsOn   []string     `json:"depends_on,omitempty"`
	WaitingOn   []string     `json:"waiting_on,omitempty"`
	Blockers    []string     `json:"blockers,omitempty"`
	UnblockWhen string       `json:"unblock_when,omitempty"`
	Attempt     *AttemptView `json:"attempt,omitempty"`
}

// BlockedView describes an explicitly blocked task with its reasons.
type BlockedView struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Reasons     []string `json:"reasons"`
	UnblockWhen string   `json:"unblock_when,omitempty"`
}

// PaceView counts recorded integrations, independent of the bounded event tail.
type PaceView struct {
	Session         int    `json:"session"`
	Hour            int    `json:"hour"`
	Day             int    `json:"day"`
	LastIntegration string `json:"last_integration,omitempty"`
}

// TaskDetail is loaded on demand through a read-only observer.
type TaskDetail struct {
	Task          TaskView
	Attempts      []AttemptView
	Summary       string
	Files         []string
	Feedback      string
	VerifyCommand string
	Checks        []CheckView
}

type CheckView struct {
	Time     string
	Kind     string
	Result   string
	Revision string
	Artifact string
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
