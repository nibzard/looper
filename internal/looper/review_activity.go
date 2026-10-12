package looper

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Review uses its normal tools and result contract. Only public process
// metadata reaches the journal; log contents and private reasoning stay local.
func (e *Engine) runReviewAgent(ctx context.Context, op operation, work, dir, prompt string, heartbeat time.Duration) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type result struct {
		raw json.RawMessage
		err error
	}
	done := make(chan result, 1)
	log := filepath.Join(dir, "events-00.jsonl")
	e.activityLog = log
	e.eventData("review-activity", "Starting review agent. Log: "+log, map[string]any{"session": e.Store.session, "phase": "review", "log": log})
	if e.err != nil {
		return nil, e.err
	}
	e.publishView(false)
	defer func() {
		e.activityLog, e.activityDetail, e.activityUpdated = "", "", ""
		e.publishView(false)
	}()
	go func() {
		raw, err := op.agent.Run(ctx, op.config.ReviewAgent, work, dir, prompt, false)
		done <- result{raw, err}
	}()
	finished := false
	defer func() {
		cancel()
		if !finished {
			<-done
		}
	}()
	tick := time.NewTicker(heartbeat)
	defer tick.Stop()
	started := time.Now()
	for {
		select {
		case r := <-done:
			finished = true
			if e.err != nil {
				return nil, e.err
			}
			return r.raw, r.err
		case <-tick.C:
			var bytes int64
			var last time.Time
			paths, _ := filepath.Glob(filepath.Join(dir, "events-*.jsonl"))
			for _, path := range paths {
				if info, err := os.Stat(path); err == nil {
					bytes += info.Size()
					if info.ModTime().After(last) {
						last, log = info.ModTime(), path
					}
				}
			}
			e.activityLog = log
			if !last.IsZero() {
				e.activityUpdated = last.UTC().Format(time.RFC3339)
			}
			e.activityDetail = "Waiting for review agent"
			e.eventData("review-heartbeat", fmt.Sprintf("Controller waiting for review (%s). Recorded output: %d bytes. Log: %s", time.Since(started).Round(time.Second), bytes, log), map[string]any{"session": e.Store.session, "phase": "review", "log": log, "output_bytes": bytes, "last_output_at": e.activityUpdated})
			if e.err != nil {
				cancel()
			}
			e.publishView(false)
		}
	}
}
