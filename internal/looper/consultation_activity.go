package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"
)

// consultationHeartbeat is the controller heartbeat interval while a
// synchronous consultation runs, matching the planner heartbeat cadence.
const consultationHeartbeat = 30 * time.Second

// consultation names one synchronous owner-goroutine call to the review agent
// that blocks the run loop: a repair diagnosis, a run-recovery consultation,
// or an import analysis. The fields carry presentation only. The agent always
// keeps its normal review tools and result contract.
type consultation struct {
	// state publishes on the view while the consultation runs.
	state string
	// startDetail is the activity detail before the agent stream has output.
	startDetail string
	// openKind and openMessage announce the consultation; the stream path is
	// appended so an operator can open the log.
	openKind, openMessage string
	// actionKind journals each readable agent action from the stream.
	actionKind string
	// waitKind journals the controller heartbeat. waitLabel names the
	// consultation inside its message.
	waitKind, waitLabel string
	// identity carries the structured identity of the consultation's own
	// events beyond the session, which runConsultation adds itself: the task
	// and attempt a consultation diagnoses, or the request it serves.
	identity eventIdentity
	// heartbeat is the resolved cadence; zero keeps consultationHeartbeat.
	heartbeat time.Duration
}

// runConsultation runs one synchronous consultation agent call with the same
// activity plumbing as ordinary planning: activity fields on the published
// view, the newest agent stream path, readable action detail, and controller
// heartbeats. A long consultation stays observable instead of presenting a
// silent loop. The consultation blocks the loop, so worker activity keeps
// publishing here: a mid-repair consultation never hides other workers.
func (e *Engine) runConsultation(ctx context.Context, op operation, work, dir, prompt string, c consultation) (json.RawMessage, error) {
	type result struct {
		raw json.RawMessage
		err error
	}
	done := make(chan result, 1)
	e.activity, e.activityDetail = c.state, c.startDetail
	e.activityAgent = op.config.ReviewAgent
	e.activityLog = filepath.Join(dir, "events-00.jsonl")
	session := ""
	if e.controller != nil {
		session = e.controller.Session
	}
	identity := c.identity
	identity.Session = session
	e.eventData(c.openKind, c.openMessage+". Log: "+e.activityLog, identity.merge(map[string]any{"log": e.activityLog}))
	// The loop owns the drain flag; a consultation that blocks it keeps
	// reporting the same request so a stop stays visible.
	e.publishView(e.drainingNow())
	defer func() {
		e.activity, e.activityDetail, e.activityLog, e.activityUpdated, e.activityAgent = "", "", "", "", ""
		e.publishView(e.drainingNow())
	}()
	go func() {
		raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, dir, prompt, false)
		done <- result{raw, err}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	started := time.Now()
	seconds := 0
	heartbeat := c.heartbeat
	if heartbeat <= 0 {
		heartbeat = consultationHeartbeat
	}
	every := int(max(time.Second, heartbeat).Seconds())
	reader := plannerActivityReader{}
	poll := func() {
		if log := newestAgentLog(dir, e.activityLog); log != e.activityLog {
			// A retry writes a fresh stream; the reader restarts with it.
			reader, e.activityLog, e.activityUpdated = plannerActivityReader{}, log, ""
		}
		if reader.poll(e.activityLog, func(action string) {
			e.activityDetail = action
			e.eventData(c.actionKind, action, identity.merge(nil))
		}) {
			e.activityUpdated = time.Now().UTC().Format(time.RFC3339)
		}
	}
	for {
		select {
		case r := <-done:
			poll()
			return r.raw, r.err
		case <-tick.C:
			poll()
			// The loop would publish this evidence itself; the consultation
			// blocks it, so the heartbeat tick carries it.
			e.pollWorkerActivity()
			seconds++
			if seconds%every == 0 {
				e.eventData(c.waitKind, fmt.Sprintf("Controller waiting for %s (%s). Last activity: %s", c.waitLabel, time.Since(started).Round(time.Second), e.activityDetail), identity.merge(map[string]any{"log": e.activityLog}))
			}
			e.publishView(e.drainingNow())
		}
	}
}
