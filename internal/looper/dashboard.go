package looper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/nibzard/looper/internal/ui"
)

// runTUI starts or attaches a dashboard. With --plain, --json, or non-terminal
// streams it falls back to the CLI for a new controller and to event
// following for an existing one; both fallbacks keep the invocation's parsed
// flags. An owned repository receives the startup plans before the fallback
// follows, with the receipt on stderr so the followed event stream stays one
// document per line. An unattached start asks the first-run form before the
// controller takes the repository lock, under the same suppression and
// cancellation rules as run and start. It then starts a daemon controller
// and attaches to it, so the dashboard can detach while the run continues;
// --foreground keeps the controller inside the dashboard process instead.
func runTUI(args []string, c Config, out, stderr io.Writer) int {
	if c.JSON || c.Plain || DetectMode(os.Stdin, out, false) != ModeInteractive {
		if repositoryOwned(c) {
			// The startup plans reach the owning controller through the same
			// receipt-returning submission the interactive attach uses; the
			// receipt prints on stderr because stdout is the followed event
			// stream. A refused submission returns without following, so the
			// plans are never dropped silently.
			if len(c.Plans) > 0 {
				if code := submitPlansToActive(c, stderr, stderr); code != 0 {
					return code
				}
			}
			fmt.Fprintln(stderr, "tui needs an interactive terminal; following events instead.")
			return followEvents(c, out, stderr)
		}
		fmt.Fprintln(stderr, "tui needs an interactive terminal; running with CLI output.")
		// The parsed configuration keeps the invocation's flags — including
		// --plain and --json, which must still suppress the first-run form.
		c.Command = "run"
		return runCLIController(args, c, out, stderr)
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
	// The unattached tui start is an eligible controller start: the form runs
	// before the dashboard controller exists, and cancelling it exits without
	// saving answers or starting work.
	c, start, err := maybeFirstRun(args, c, os.Stdin, out, stderr)
	if err != nil {
		return emitParseError(args, err, out, stderr)
	}
	if !start {
		return 0
	}
	if c.Foreground {
		return runDashboardController(c, out, stderr, ui.RunDashboard)
	}
	return startAndAttach(c, out, stderr)
}

// startAndAttach starts a daemon controller and attaches the dashboard to it
// with controls. The start report is held back, because the dashboard would
// cover it; it prints only when the start fails or the run already ended.
func startAndAttach(c Config, out, stderr io.Writer) int {
	var started bytes.Buffer
	if code := StartDetached(c, &started, stderr); code != 0 {
		out.Write(started.Bytes())
		return code
	}
	if !repositoryOwned(c) {
		// A short run, such as --once, can finish before the attach.
		out.Write(started.Bytes())
		return 0
	}
	return runAttach(c, out, stderr, true)
}

func runDashboardController(c Config, out, stderr io.Writer, runDashboard func(ui.Controls) (ui.ExitAction, error)) int {
	_, dir := projectPaths(c)
	// Main dispatches tui before its controller signal block, so this
	// controller installs the same handlers itself: SIGTERM drains like
	// `looper stop` without cancelling, and SIGINT cancels with retained
	// work. Signals install before the lock, exactly like the daemon path, so
	// an early drain request stays pending until the loop reaches its first
	// boundary. The dashboard keeps Bubble Tea's signal capture off, so these
	// handlers own the signals and the board exits through its Done poll when
	// the drained or cancelled run ends.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	installControllerSignals(c, cancel)
	var engineOut bytes.Buffer
	e, lock, err := NewEngine(c, &engineOut)
	if err != nil {
		return emitError(c, err, out, stderr)
	}
	info := e.controller
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
		NoAnimation: c.NoAnimation,
		OwnSignals:  true,
		Statistics:  func() (ui.StatisticsView, error) { return observer.store.Statistics(time.Now()) },
		Inspect:     observer.inspect,
		WorkerLog:   func(id string) (string, string, error) { return workerLog(dir, id) },
		Log:         func(path string) (string, error) { return plannerLog(dir, path) },
		Snapshot:    e.viewNotify,
		Poll:        func() *ui.ViewSnapshot { return observer.view(dir, info.Session) },
		Events:      observer.events,
		Diff:        func(id string) (string, error) { return attemptDiff(dir, id) },
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
		// The request binds to this controller's session, exactly like
		// `looper stop`, so it can never outlive the run that should drain.
		return writeStopRequest(dir, stopRequest{Session: info.Session, PID: info.PID})
	}
	controls.Drain = drain
	controls.Requeue = func(taskID, revision string) error { return requeueTask(dir, info.Session, taskID, revision) }
	controls.AddPlan = func(path string) (string, error) {
		planPath, err := filepath.Abs(path)
		if err != nil {
			return "", err
		}
		// The published snapshot carries each receipt's state, so the
		// dashboard follows the import without polling its acknowledgement.
		req, err := submitPlanImportRequest(dir, info.Session, "", []string{planPath})
		if err != nil {
			return "", err
		}
		return req.ID, nil
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
	// Bubble Tea has restored the normal terminal by now. Keep the outcome
	// visible in scrollback instead of leaving it only in the alternate screen.
	printDashboardExit(out, observer.store, info.Session, finish.code)
	return finish.code
}

func printDashboardExit(out io.Writer, store *Store, session string, code int) {
	var raw string
	var exit sessionExit
	err := store.db.QueryRow("SELECT data FROM events WHERE kind='shutdown' AND json_extract(data, '$.session')=? ORDER BY id DESC LIMIT 1", session).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &exit)
	}
	if err != nil || exit.Session != session || exit.ExitCode != code {
		fmt.Fprintf(out, "Looper exited with code %d. Run looper events to inspect the exit details.\n", code)
		return
	}
	switch exit.Reason {
	case "drained":
		fmt.Fprintln(out, "Drain complete. Active work finished; Looper stopped cleanly.")
		fmt.Fprintln(out, "Run looper tui again with your usual agent settings to resume remaining work.")
	case "cancelled":
		fmt.Fprintln(out, "Interrupted. Work is retained. Run looper tui with your usual agent settings to resume.")
	case "complete":
		fmt.Fprintln(out, "Looper finished: all planned work is complete.")
	case "no_runnable_tasks":
		fmt.Fprintln(out, "Looper stopped: no runnable tasks. Run looper status to review remaining work.")
	case "dispatch_limit":
		fmt.Fprintln(out, "Looper stopped: dispatch limit reached. Run looper tui to continue.")
	default:
		fmt.Fprintf(out, "Looper stopped: %s (exit %d). Run looper events for details.\n", sanitize(exit.Reason), code)
	}
}

// submitPlansToActive submits startup plans to the controller that already
// owns the repository, through the same receipt-returning submission the CLI
// mutation uses (AX-011). receiptOut is where the immediate receipt prints:
// the interactive tui names its stdout, and the non-interactive fallback names
// its stderr so the followed event stream stays one event document per line.
func submitPlansToActive(c Config, receiptOut, stderr io.Writer) int {
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		fmt.Fprintln(stderr, "A controller owns this repository, but its metadata cannot be verified:", err)
		return 1
	}
	req, err := submitPlanImportRequest(dir, info.Session, "", c.Plans)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	return emitMutationReceipt(c, newMutationReceipt(req.ID, PublicRequestAddPlan, RequestSubmitted, req.CreatedAt, info.Session),
		"Queued "+strings.Join(c.Plans, ", "), drainingImportNote(dir, info), receiptOut)
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
		p := runNotFoundError("", "looper")
		p.Message = "No active run. Start one with: looper"
		p.Paths = nil
		p.withLegacy("No active run. Start one with: looper", 1)
		return emitError(c, p, out, stderr)
	}
	_, dir := projectPaths(c)
	info, err := readController(dir)
	if err != nil {
		p := controllerMetadataError(err)
		p.withLegacy(fmt.Sprintf("A controller owns this repository, but its metadata cannot be verified: %s", err), 1)
		return emitError(c, p, out, stderr)
	}
	if c.JSON || c.Plain || DetectMode(os.Stdin, out, false) != ModeInteractive {
		fmt.Fprintln(stderr, "attach needs an interactive terminal; following events instead.")
		return followEvents(c, out, stderr)
	}
	observer, err := openObserver(dir)
	if err != nil {
		fmt.Fprintln(stderr, "Error:", err)
		return 1
	}
	defer observer.Close()
	attachment := &dashboardAttachment{observer: observer, dir: dir, session: info.Session}
	controls := ui.Controls{
		NoAnimation: c.NoAnimation,
		Statistics:  func() (ui.StatisticsView, error) { return observer.store.Statistics(time.Now()) },
		Inspect:     attachment.inspect,
		WorkerLog:   func(id string) (string, string, error) { return workerLog(dir, id) },
		Log:         func(path string) (string, error) { return plannerLog(dir, path) },
		Poll:        attachment.poll,
		Events:      observer.events,
		ReadOnly:    !withControls,
		Diff:        func(id string) (string, error) { return attemptDiff(dir, id) },
	}
	if withControls {
		controls.Requeue = func(taskID, revision string) error {
			session, err := attachment.currentSession()
			if err != nil {
				return err
			}
			return requeueTask(dir, session, taskID, revision)
		}
		controls.Drain = func() error {
			session, err := attachment.currentSession()
			if err != nil {
				return err
			}
			// Bind the request to the attached controller's session so it
			// cannot drain a future run if that controller exits first.
			return writeStopRequest(dir, stopRequest{Session: session})
		}
		controls.AddPlan = attachment.addPlan
		controls.Interrupt = attachment.interrupt
	}
	for {
		action, err := ui.RunDashboard(controls)
		if err != nil {
			fmt.Fprintln(stderr, "Error:", err)
			return 1
		}
		if action.Kind != "settings" {
			printDetach(out, dir, attachment.lastSession(), action.Kind)
			break
		}
		session, err := attachment.currentSession()
		if err != nil {
			controls.Notice = err.Error()
			continue
		}
		notice, err := submitSettingsFromForm(c, dir, session, os.Stdin)
		if err != nil {
			controls.Notice = "settings: " + err.Error()
		} else {
			controls.Notice = notice
		}
	}
	return 0
}

// printDetach tells the operator what continues after the dashboard exits.
func printDetach(out io.Writer, dir, session, kind string) {
	switch kind {
	case "drain":
		fmt.Fprintln(out, "Drain requested. The controller finishes active work, then stops. Check it with `looper status`.")
	case "interrupt":
		fmt.Fprintf(out, "Interrupt sent to session %s. Active work stops and is retained; run looper tui to resume.\n", session)
	default:
		if info, err := readController(dir); err == nil && info.Session == session && dashboardControllerAlive(info) {
			fmt.Fprintf(out, "Looper keeps running in the background, session %s. Reattach with `looper tui`; drain it with `looper stop`.\n", session)
		}
	}
}

// followEvents runs the tail command with the parsed configuration. The tui
// and attach fallbacks re-entered Main with the fixed argument list
// ["--tail", "--follow"], so Parse rebuilt defaults and --json, --plain, and
// the contract selection were dropped from the followed stream; switching the
// parsed configuration to tail keeps every flag (T022).
func followEvents(c Config, out, stderr io.Writer) int {
	c.Command, c.Follow = "tail", true
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := inspect(ctx, c, out); err != nil {
		return emitError(c, err, out, stderr)
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

// openObserver attaches the dashboard's event reader. The dashboard holds
// this connection for its whole session — an attach survives the controller
// transitions it observes — so it keeps the through-WAL attach instead of the
// immutable read a stopped run allows (T105).
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
		var invalid *cursorInvalidError
		if after < 0 || !errors.As(err, &invalid) {
			return nil
		}
		// The retained store was reset or replaced while the dashboard was
		// attached. Restart the tail from the newest events instead of
		// freezing the journal on a cursor that no longer exists.
		if events, err = o.store.RecentEvents(-1, limit); err != nil {
			return nil
		}
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
			if os.IsNotExist(err) && a.Phase == "done" {
				return fmt.Sprintf("%s completed. Its temporary worktree was cleaned up after integration.\nA live diff is no longer available. Press [esc] to return, then [l] for retained logs.", a.TaskID), nil
			}
			return "", fmt.Errorf("%s diff unavailable: cannot access its worktree: %w", a.TaskID, err)
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
		// Patch content is worker-written file text. Escape its control
		// characters for this human view, like readDashboardLog's raw fallback.
		// The retained worktree keeps the original bytes.
		return sanitize(string(patch)), nil
	}
	return "", fmt.Errorf("unknown attempt %s", attemptID)
}

// attemptDirName matches the runtime attempt directories that own a repair
// diagnosis stream beside the attempt's worker and check records.
var attemptDirName = regexp.MustCompile(`^task-[0-9a-f]{16}$`)

// plannerLog accepts only controller agent output files within this runtime
// directory: planner, review, bootstrap, and import-analysis streams, plus a
// repair diagnosis stream inside its attempt directory.
func plannerLog(dir, path string) (string, error) {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return "", err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	// A diagnosis consultation records under the attempt it repairs, so its
	// stream sits one directory deeper than every other controller stream.
	if len(parts) == 3 && attemptDirName.MatchString(parts[0]) {
		parts = parts[1:]
	}
	if len(parts) != 2 || !controllerStreamDir(parts[0]) ||
		!strings.HasPrefix(parts[1], "events-") || !strings.HasSuffix(parts[1], ".jsonl") {
		return "", fmt.Errorf("invalid controller log path")
	}
	return readDashboardLog(path)
}

// controllerStreamDir names the runtime directories that hold controller
// agent streams a dashboard may open.
func controllerStreamDir(name string) bool {
	for _, prefix := range []string{"plan-", "review-", "bootstrap-", "import-analysis-", "run-repair-", "diagnose-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
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
	structured := false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		structured = structured || json.Valid(line)
		if actions := plannerActivity(line); len(actions) > 0 {
			readable = append(readable, actions...)
		}
	}
	if len(readable) > 0 {
		return strings.Join(readable, "\n"), err
	}
	if strings.HasSuffix(path, ".jsonl") && structured {
		return "No readable actions in the latest agent output. Progress is unknown.", err
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
