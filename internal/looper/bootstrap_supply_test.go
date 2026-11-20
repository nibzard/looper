package looper

// T144: a task file the operator supplies while the startup bootstrap agent
// call itself runs. T141 adopted such a file only when it appeared during the
// quota wait it added; the agent call kept a minutes-long window whose
// completed bootstrap result overwrote the operator's bytes. The adoption now
// covers both windows with the same journal event, the same staging release
// for supplied startup plans, and the same ordinary startup parse of the
// operator's file.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bootstrapSuppliedBacklog is the operator's hand-written task file: one task
// done from the start, so a once run needs no worker.
const bootstrapSuppliedBacklog = `{"schema_version":1,"goal":"operator backlog","source_files":[],"tasks":[{"id":"T001","title":"operator task","priority":1,"status":"done","details":"supplied while the bootstrap agent call ran","depends_on":[],"tags":["project-done"]}]}`

// bootstrapInvalidBacklog is a bootstrap answer the unfixed path refuses —
// it lists a private context source — so a run that adopts it proves the
// guard sits before validation.
const bootstrapInvalidBacklog = `{"schema_version":1,"goal":"quota startup","source_files":["to-do.json.context/bootstrap-old/backlog.json"],"tasks":[{"id":"T001","title":"one","priority":1,"status":"todo","details":"invalid","depends_on":[]}]}`

// bootstrapSupplyStub writes one stub agent for the fresh-project startup
// path. Its bootstrap invocation first writes the operator's task file — the
// supply happens while the agent call runs — and then answers, so the
// completed bootstrap result competes with the operator's bytes. answer is
// the literal bootstrap result: the ordinary bootstrapped backlog, or
// bootstrapInvalidBacklog for the pre-validation route. Every bootstrap
// invocation appends to the call log, so a test can prove no correction
// round followed an adoption. Review prompts answer complete and planning
// prompts answer a trivial plan, like bootstrapQuotaStub.
func bootstrapSupplyStub(t *testing.T, name, answer string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, name)
	script := `#!/bin/bash
out=""; prev=""
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then out="$a"; fi
  prev="$a"
done
p="$(cat)"
result=''
if printf '%s' "$p" | grep -q "Bootstrap a development backlog"; then
  printf '%s\n' bootstrap >> "$T144_CALLS"
  sleep 1
  printf '%s' '` + bootstrapSuppliedBacklog + `' > "$T144_TODO"
  result='` + answer + `'
elif printf '%s' "$p" | grep -q "review pass for Looper"; then
  result='` + reviewComplete + `'
elif printf '%s' "$p" | grep -q "Plan execution for Looper"; then
  result='` + bootstrapQuotaPlan + `'
fi
if [ -n "$result" ]; then
  printf '{"structured_output":%s}\n' "$result"
  if [ -n "$out" ]; then printf '%s' "$result" > "$out"; fi
fi
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// bootstrapSupplyRun starts one controller on a fresh project whose stub
// supplies the operator's task file during the bootstrap agent call. plans
// optionally supplies one startup plan, so the test also covers the staging
// record the adoption must release. It returns the engine, its config, the
// exit channel, the call log, and stop.
func bootstrapSupplyRun(t *testing.T, bin string, plans int) (*Engine, Config, chan bootstrapExit, string, func()) {
	t.Helper()
	calls := filepath.Join(t.TempDir(), "calls")
	e, c, done, stop := bootstrapQuotaRun(t, bin, false, func(cfg *Config) {
		t.Setenv("T144_TODO", cfg.Todo)
		t.Setenv("T144_CALLS", calls)
		if plans > 0 {
			plan := filepath.Join(t.TempDir(), "plan.md")
			if err := os.WriteFile(plan, []byte("# Extra requirement\nAdopt the operator backlog.\n"), 0600); err != nil {
				t.Fatal(err)
			}
			cfg.Plans = []string{plan}
		}
	})
	return e, c, done, calls, stop
}

// bootstrapSupplyAssert checks the adoption outcome the two routes share:
// the start completes, the operator's bytes stay the task file and the saved
// baseline, and the bootstrap journal names the adoption. It returns the
// journal snapshot and the saved state taken before the store closes.
func bootstrapSupplyAssert(t *testing.T, e *Engine, c Config, done chan bootstrapExit, stop func()) ([]Event, *State) {
	t.Helper()
	select {
	case exit := <-done:
		if exit.code != 0 || exit.err != nil {
			stop()
			t.Fatalf("the supplied task file must complete the start; exit %d, err %v", exit.code, exit.err)
		}
	case <-time.After(testBudget(30 * time.Second)):
		stop()
		t.Fatal("the supplied task file never completed the start")
	}
	events := quotaSnapshot(t, e)
	s, loadErr := e.Store.Load()
	stop()
	adopted := false
	for _, ev := range eventsOfKind(events, "bootstrap") {
		if strings.Contains(ev.Message, "during the bootstrap agent call") && strings.Contains(ev.Message, "adopts the operator's file") {
			adopted = true
		}
	}
	if !adopted {
		t.Fatalf("the adoption must journal its bootstrap event: %+v", eventsOfKind(events, "bootstrap"))
	}
	// The operator's bytes stay authoritative: the goal survives on disk and
	// as the saved baseline, and the bootstrap backlog never landed over them.
	data, err := os.ReadFile(c.Todo)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Backlog
	if err = json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.Goal != "operator backlog" || len(onDisk.Tasks) != 1 || onDisk.Tasks[0].Title != "operator task" {
		t.Fatalf("the operator backlog must remain the task file: %s", data)
	}
	if loadErr != nil || s == nil || len(s.Backlog.Tasks) != 1 || s.Backlog.Tasks[0].Title != "operator task" || s.Backlog.Goal != "operator backlog" {
		t.Fatalf("the operator backlog must be the saved baseline (load err %v): %+v", loadErr, s)
	}
	return events, s
}

// TestBootstrapAdoptsTaskFileSuppliedDuringAgentCall is the acceptance route:
// the operator hand-writes the task file while the bootstrap agent call runs,
// and the completed bootstrap result must not overwrite it. The start adopts
// the operator's bytes, a supplied startup plan loses its bootstrap staging
// record and follows the ordinary startup import against that backlog, and
// the once run completes over it.
func TestBootstrapAdoptsTaskFileSuppliedDuringAgentCall(t *testing.T) {
	bin := bootstrapSupplyStub(t, "bootstrap-supply", bootstrapQuotaBacklog)
	e, c, done, _, stop := bootstrapSupplyRun(t, bin, 1)
	defer stop()
	// The staging record of the captured startup plan exists while bootstrap
	// owns it; the operator's file supersedes that ownership.
	staging := ""
	waitFor(t, "the captured startup plan to stage", func() bool {
		entries, _ := os.ReadDir(importsDir(e.Dir))
		for _, entry := range entries {
			if _, statErr := os.Stat(stagingPath(e.Dir, entry.Name())); statErr == nil {
				staging = stagingPath(e.Dir, entry.Name())
				return true
			}
		}
		return false
	})
	_, s := bootstrapSupplyAssert(t, e, c, done, stop)
	if _, statErr := os.Stat(staging); !os.IsNotExist(statErr) {
		t.Fatal("the superseded staging record must be removed")
	}
	// The plan follows the ordinary startup import against that backlog.
	if len(s.Imports) == 0 {
		t.Fatalf("the supplied plan must reach the ordinary import records: %+v", s.Imports)
	}
}

// TestBootstrapAdoptsSuppliedFileBeforeTheCorrectionRound pins where the
// guard sits: the check runs when the agent call returns, before the result
// is validated. A bootstrap answer that would be refused — and so spend a
// correction round — adopts the operator's file instead, and the stub's call
// log proves bootstrap ran exactly once.
func TestBootstrapAdoptsSuppliedFileBeforeTheCorrectionRound(t *testing.T) {
	bin := bootstrapSupplyStub(t, "bootstrap-supply-invalid", bootstrapInvalidBacklog)
	e, c, done, calls, stop := bootstrapSupplyRun(t, bin, 0)
	defer stop()
	events, _ := bootstrapSupplyAssert(t, e, c, done, stop)
	if corrections := len(eventsOfKind(events, "bootstrap-correction")); corrections != 0 {
		t.Fatalf("an adopted file must not spend a correction round: %d correction event(s)", corrections)
	}
	log, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(log), "bootstrap"); n != 1 {
		t.Fatalf("bootstrap must run exactly once over the supplied file, ran %d time(s)", n)
	}
}
