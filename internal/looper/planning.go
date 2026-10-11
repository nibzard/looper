package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// runPlanner keeps presentation updates on the owner goroutine while the
// agent runs. Cancellation waits for the child to exit before workspace cleanup.
func (e *Engine) runPlanner(ctx context.Context, op operation, work, dir, prompt string) (json.RawMessage, error) {
	type result struct {
		raw json.RawMessage
		err error
	}
	type progress struct{ detail, log string }
	updates := make(chan progress)
	done := make(chan result, 1)
	e.activity, e.activityDetail = "planning", "Starting the planning agent"
	e.publishView(false)
	defer func() {
		e.activity, e.activityDetail, e.activityLog, e.activityUpdated = "", "", "", ""
		e.publishView(false)
	}()
	go func() {
		raw, err := op.agent.run(ctx, op.config.ReviewAgent, work, dir, prompt, false, func(detail, log string) {
			select {
			case updates <- progress{detail, log}:
			case <-ctx.Done():
			}
		})
		done <- result{raw, err}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	started := time.Now()
	seconds := 0
	reader := plannerActivityReader{}
	poll := func() {
		if reader.poll(e.activityLog, func(action string) {
			e.activityDetail = action
			e.event("planner-activity", action)
		}) {
			e.activityUpdated = time.Now().UTC().Format(time.RFC3339)
		}
	}
	for {
		select {
		case r := <-done:
			poll()
			return r.raw, r.err
		case p := <-updates:
			if e.activityLog != p.log {
				reader = plannerActivityReader{}
				e.activityUpdated = ""
			}
			e.activityDetail, e.activityLog = p.detail, p.log
			e.event("plan", fmt.Sprintf("Planning: %s. Log: %s", p.detail, p.log))
			e.publishView(false)
		case <-tick.C:
			poll()
			seconds++
			if seconds%30 == 0 {
				e.event("plan", fmt.Sprintf("Controller waiting for planner (%s). Last activity: %s", time.Since(started).Round(time.Second), e.activityDetail))
			}
			e.publishView(false)
		}
	}
}
