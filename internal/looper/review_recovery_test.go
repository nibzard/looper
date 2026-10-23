package looper

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestOverdueReviewSurvivesCancellationAndRestart(t *testing.T) {
	bin, _ := fakeAgent(t, "")
	t.Setenv("LOOPER_FAKE_SLEEP", "30")
	c := repoConfig(t, "run")
	c.CodexBin, c.ClaudeBin, c.ReviewAgent, c.ReviewEvery = bin, bin, "codex", 5
	b := Backlog{Version: 1, Sources: []string{}, Tasks: []Task{{ID: "T001", Title: "next", Priority: 1, Status: "todo"}}}
	if err := writeJSON(c.Todo, b); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reproduce the old durable state: integrations saved, pending flag lost.
	e.State.Integrated, e.State.ReviewedAt, e.State.ReviewPending = 5, 0, false
	if err = e.save(); err != nil {
		t.Fatal(err)
	}
	e.Store.Close()
	lock.Close()
	for i := 0; i < 2; i++ {
		e, lock, err = NewEngine(c, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		code, runErr := e.Run(ctx)
		cancel()
		if code != 130 || !errors.Is(runErr, context.DeadlineExceeded) {
			t.Fatalf("run: %d %v", code, runErr)
		}
		saved, err := e.Store.Load()
		if err != nil {
			t.Fatal(err)
		}
		events, err := e.Store.Events(0)
		if err != nil {
			t.Fatal(err)
		}
		reviews := 0
		for _, ev := range events {
			if ev.Kind == "review" {
				reviews++
			}
		}
		if reviews != i+1 || !saved.ReviewPending || saved.ReviewedAt != 0 || len(saved.Attempts) != 0 {
			t.Fatalf("review was lost or worker dispatched: reviews=%d state=%+v", reviews, saved)
		}
		e.Store.Close()
		lock.Close()
	}

	// Complete the review on the next restart and prove dispatch resumes.
	t.Setenv("LOOPER_FAKE_SLEEP", "")
	t.Setenv("LOOPER_FAKE_REVIEW", `{"status":"reviewed","summary":"checked","findings":[],"complete":false}`)
	t.Setenv("LOOPER_FAKE_PLAN", `{"setup":"","verify":"true","tasks":[{"task_id":"T001","write_paths":["."],"resources":[]}]}`)
	t.Setenv("LOOPER_FAKE_SUMMARY", `{"task_id":"T001","status":"blocked","summary":"needs fixture input","files":[],"blockers":["fixture input"],"followups":[],"unblock_when":"provide fixture"}`)
	c.Once = true
	e, lock, err = NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Store.Close()
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 2 || err != nil || e.State.ReviewedAt != 5 || e.State.ReviewPending || len(e.State.Attempts) != 1 {
		t.Fatalf("did not resume after review: code=%d err=%v state=%+v", code, err, e.State)
	}

}

func TestReviewDueAndDispatchReason(t *testing.T) {
	e := snapshotEngine(t)
	e.State.Attempts = nil
	e.State.Finished = false
	for _, tc := range []struct {
		every, integrated, reviewed int
		pending, want               bool
	}{
		{5, 5, 0, false, true}, {5, 5, 5, false, false}, {0, 5, 0, false, false}, {0, 5, 0, true, true}, {3, 5, 0, false, true},
	} {
		e.Config.ReviewEvery = tc.every
		e.State.Integrated, e.State.ReviewedAt, e.State.ReviewPending = tc.integrated, tc.reviewed, tc.pending
		if e.reviewDue() != tc.want {
			t.Fatalf("due: %+v", tc)
		}
		v := e.snapshot(false)
		if tc.want && v.DispatchReason != "Milestone review is due before dispatch" {
			t.Fatalf("reason: %+v", v)
		}
	}
}
