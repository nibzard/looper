package looper

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// runTUI starts or attaches a dashboard. With --plain, --json, or non-terminal
// streams it falls back to the CLI for a new controller and to event
// following for an existing one.
func runTUI(c Config, out, stderr io.Writer) int {
	if c.JSON || c.Plain || DetectMode(os.Stdin, out, false) != ModeInteractive {
		if repositoryOwned(c) {
			fmt.Fprintln(stderr, "tui needs an interactive terminal; following events instead.")
			c.Command, c.Follow = "tail", true
			return Main([]string{"--tail", "--follow"}, out, stderr)
		}
		fmt.Fprintln(stderr, "tui needs an interactive terminal; running with CLI output.")
		return Main(nil, out, stderr)
	}
	if repositoryOwned(c) {
		// tui attaches to the controller it finds and submits its plans
		// through the same importer.
		if len(c.Plans) > 0 {
			if code := submitPlansToActive(c, out, stderr); code != 0 {
				return code
			}
		}
		return runAttach(c, out, stderr, true)
	}
	return runDashboardController(c, out, stderr, ui.RunDashboard)
}

func runDashboardController(c Config, out, stderr io.Writer, runDashboard func(ui.Controls) (ui.ExitAction, error)) int {
	_, dir := projectPaths(c)
	var engineOut bytes.Buffer
	e, lock, err := NewEngine(c, &engineOut)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	info := e.controller
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		code int
		err  error
	}
	var finish result
	done := make(chan struct{})
	go func() {
		code, err := e.Run(ctx)
		lock.Close()
		e.Store.Close()
		finish = result{code, err}
		close(done)
	}()
	observer, err := openObserver(dir)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		cancel()
		<-done
		return 1
	}
	defer observer.Close()
	controls := ui.Controls{
		Statistics: func() (ui.StatisticsView, error) { return observer.store.Statistics(time.Now()) },
		Inspect:    observer.inspect,
		WorkerLog:  func(id string) (string, string, error) { return workerLog(dir, id) },
		Log:        func(path string) (string, error) { return plannerLog(dir, path) },
		Snapshot:   e.viewNotify,
		Poll:       func() *ui.ViewSnapshot { return observer.view(dir, info.Session) },
		Events:     observer.events,
		Diff:       func(id string) (string, error) { return attemptDiff(dir, id) },
		Done: func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		},
		Cancel: cancel,
	}
	drain := func() error {
		return atomicWrite(filepath.Join(dir, "stop"), []byte("drain\n"), 0600)
	}
	controls.Drain = drain
	controls.Requeue = func(taskID, revision string) error { return requeueTask(dir, info.Session, taskID, revision) }
	controls.AddPlan = func(path string) error {
		planPath, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		id, err := submitPlanImport(dir, info.Session, []string{planPath})
		if err != nil {
			return err
		}
		go waitForPlanImport(dir, id, time.Hour)
		return nil
	}
	for {
		action, err := runDashboard(controls)
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			cancel()
			<-done
			return 1
		}
		switch action.Kind {
		case "settings":
			notice, err := submitSettingsFromForm(c, dir, info.Session, os.Stdin)
			if err != nil {
				controls.Notice = "settings: " + err.Error()
			} else {
				controls.Notice = notice
			}
			continue
		}
		break
	}
	<-done
	if finish.err != nil && ctx.Err() == nil {
		fmt.Fprintln(stderr, "Error:", finish.err)
	}
	return finish.code
}

// submitPlansToActive submits startup plans to the controller that already
// owns the repository.
func submitPlansToActive(c Config, out, stderr io.Writer) int {
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		fmt.Fprintln(stderr, "A controller owns this repository, but its metadata cannot be verified:", err)
		return 1
	}
	id, err := submitPlanImport(dir, info.Session, c.Plans)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	fmt.Fprintln(out, "Queued plan import", id)
	return 0
}

// submitSettingsFromForm runs the full editor against the current profile and
// submits the changes through the controller request protocol.
func submitSettingsFromForm(c Config, dir, session string, in *os.File) (string, error) {
	profile, err := LoadProfile(c.ProfilePath)
	if err != nil {
		return "", err
	}
	before := settingsFromProfile(profile)
	s := before
	if err = ui.RunEditorForm(&s); err != nil {
		return "", err
	}
	fields := changedFields(before, s)
	if len(fields) == 0 {
		return "settings: no changes", nil
	}
	ack, err := submitSettingsRequest(dir, SettingsRequest{
		Version: SettingsRequestVersion, Session: session,
		ProfileDigest: profileDigest(c.ProfilePath), Fields: fields,
	}, 30*time.Second)
	if err != nil {
		return "", err
	}
	if ack.Status != AckApplied {
		return "", fmt.Errorf("%s: %s", ack.Status, ack.Message)
	}
	keys := make([]string, 0, len(ack.Outcomes))
	for key := range ack.Outcomes {
		keys = append(keys, key)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+": "+ack.Outcomes[key])
	}
	return "settings: " + strings.Join(parts, "; "), nil
}

// runAttach observes an active controller without starting one. With controls,
// tui attaches through the same request protocol; plain attach stays read-only.
func runAttach(c Config, out, stderr io.Writer, withControls bool) int {
	if !repositoryOwned(c) {
		fmt.Fprintln(stderr, "No active run. Start one with: looper")
		return 1
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		fmt.Fprintln(stderr, "A controller owns this repository, but its metadata cannot be verified:", err)
		return 1
	}
	if c.JSON || c.Plain || DetectMode(os.Stdin, out, false) != ModeInteractive {
		fmt.Fprintln(stderr, "attach needs an interactive terminal; following events instead.")
		return Main([]string{"--tail", "--follow"}, out, stderr)
	}
	observer, err := openObserver(dir)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer observer.Close()
	controls := ui.Controls{
		Statistics: func() (ui.StatisticsView, error) { return observer.store.Statistics(time.Now()) },
		Inspect:    observer.inspect,
		WorkerLog:  func(id string) (string, string, error) { return workerLog(dir, id) },
		Log:        func(path string) (string, error) { return plannerLog(dir, path) },
		Poll:       func() *ui.ViewSnapshot { return observer.view(dir, info.Session) },
		Events:     observer.events,
		ReadOnly:   !withControls,
		Diff:       func(id string) (string, error) { return attemptDiff(dir, id) },
	}
	if withControls {
		controls.Requeue = func(taskID, revision string) error { return requeueTask(dir, info.Session, taskID, revision) }
		controls.Drain = func() error {
			current, err := readController(dir)
			if err != nil || current.Session != info.Session || !dashboardControllerAlive(current) {
				return fmt.Errorf("The controller session changed. Detach and reconnect before requesting a drain.")
			}
			return atomicWrite(filepath.Join(dir, "stop"), []byte("drain\n"), 0600)
		}
		controls.AddPlan = func(path string) error {
			absolute, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			_, err = submitPlanImport(dir, info.Session, []string{absolute})
			return err
		}
	}
	for {
		action, err := ui.RunDashboard(controls)
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		if action.Kind != "settings" {
			break
		}
		notice, err := submitSettingsFromForm(c, dir, info.Session, os.Stdin)
		if err != nil {
			controls.Notice = "settings: " + err.Error()
		} else {
			controls.Notice = notice
		}
	}
	return 0
}

// observer pages the event journal through a read-only store connection.
type observer struct {
	store       *Store
	pace        *ui.PaceView
	paceAt      time.Time
	paceSession string
	outcomes    []ui.JournalEvent
}

func openObserver(dir string) (*observer, error) {
	store, err := OpenStoreReadonly(filepath.Join(dir, "state.db"))
	if err != nil {
		return nil, err
	}
	return &observer{store: store}, nil
}

func (o *observer) Close() error { return o.store.Close() }

func (o *observer) events(after int64, limit int) []ui.JournalEvent {
	events, err := o.store.RecentEvents(after, limit)
	if err != nil {
		return nil
	}
	out := make([]ui.JournalEvent, 0, len(events))
	for _, e := range events {
		out = append(out, ui.JournalEvent{ID: e.ID, Time: e.Time, Kind: e.Kind, Message: e.Message})
	}
	return out
}

// attemptDiff reads the selected attempt's worktree from the published
// snapshot, with external diff helpers and text conversion disabled and
// bounded output. A removed worktree is reported, not fatal.
func attemptDiff(dir, attemptID string) (string, error) {
	v := LoadView(dir)
	if v == nil {
		return "", fmt.Errorf("no published dashboard state")
	}
	for _, a := range dashboardAttempts(v) {
		if a.ID != attemptID {
			continue
		}
		if a.Worktree == "" || a.Base == "" {
			return "", fmt.Errorf("the attempt has no worktree to diff")
		}
		if _, err := os.Stat(a.Worktree); err != nil {
			return "", fmt.Errorf("the worktree was removed")
		}
		patch, err := gitCommand(a.Worktree, nil, nil, "diff", "--no-ext-diff", "--no-textconv", a.Base, "--")
		if err != nil {
			return "", fmt.Errorf("cannot read the worktree diff: %w", err)
		}
		const cap = 128 << 10
		if len(patch) > cap {
			patch = append(patch[:cap:cap], []byte("\n... diff truncated ...\n")...)
		}
		if len(patch) == 0 {
			patch = []byte("(no changes against the integration base yet)")
		}
		return string(patch), nil
	}
	return "", fmt.Errorf("unknown attempt %s", attemptID)
}

// plannerLog accepts only planner output files within this runtime directory.
func plannerLog(dir, path string) (string, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || (!strings.HasPrefix(parts[0], "plan-") && !strings.HasPrefix(parts[0], "review-")) ||
		!strings.HasPrefix(parts[1], "events-") || !strings.HasSuffix(parts[1], ".jsonl") {
		return "", fmt.Errorf("invalid controller log path")
	}
	return readDashboardLog(path)
}

func readDashboardLog(path string) (string, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if _, err = f.Seek(max(0, info.Size()-16000), io.SeekStart); err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 16000))
	var readable []string
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if actions := plannerActivity(line); len(actions) > 0 {
			readable = append(readable, actions...)
		}
	}
	if len(readable) > 0 {
		return strings.Join(readable, "\n"), err
	}
	return sanitize(string(data)), err
}

// workerLog resolves logs from the published attempt, never from an arbitrary
// path supplied by the viewer. Only known output locations are searched.
func workerLog(dir, id string) (string, string, error) {
	view := LoadView(dir)
	if view == nil {
		return "", "", fmt.Errorf("No active run. Return to the board for status.")
	}
	var attempt *ui.AttemptView
	attempts := dashboardAttempts(view)
	for i := range attempts {
		if attempts[i].ID == id {
			attempt = &attempts[i]
			break
		}
	}
	if attempt == nil {
		return "", "", fmt.Errorf("No retained logs for this attempt. Inspect the task for its result.")
	}
	root := attempt.LogDir
	if root == "" {
		if filepath.Base(id) != id || !strings.HasPrefix(id, "task-") {
			return "", "", fmt.Errorf("No log directory for this attempt.")
		}
		root = filepath.Join(dir, id)
	}
	patterns := []string{"work-*/setup.log", "work-*/events-*.jsonl", "work-*/report-recovery/events-*.jsonl"}
	if attempt.Phase == "verifying" {
		patterns = []string{"checks/*/setup.log", "checks/*/verify.log"}
	}
	if attempt.Phase == "done" || attempt.Phase == "blocked" {
		patterns = append(patterns, "checks/*/setup.log", "checks/*/verify.log")
	}
	var newest string
	var stamp time.Time
	for _, pattern := range patterns {
		paths, _ := filepath.Glob(filepath.Join(root, pattern))
		for _, path := range paths {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if newest == "" || info.ModTime().After(stamp) || (info.ModTime().Equal(stamp) && path > newest) {
				newest, stamp = path, info.ModTime()
			}
		}
	}
	if newest == "" {
		return "", "Waiting for worker output. This view refreshes each second.", nil
	}
	text, err := readDashboardLog(newest)
	return newest, text, err
}
