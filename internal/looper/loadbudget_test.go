package looper

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The internal/looper suite is fsync-bound: every store commit and every
// atomic write reaches the disk before the next step, so sibling suites on
// the same host stretch each durable step by the time their own fsyncs sit
// queued ahead of it. Fixed budgets sized on a quiet host then fail healthy
// runs — the recorded T104, T106, and T108 flakes measured a 0.6 s step
// passing 10 s at load 8-10, and a 16 s step passing 60 s while /proc/
// pressure/io reported "full" stalls of 50-65 percent. testBudget grows a
// bound with that pressure, so a healthy path that is merely slow still
// passes while a condition that never arrives still fails at the scaled
// bound.

// maxBudgetFactor caps the growth so a wedged host still fails tests inside
// the go test timeout. The worst recorded pressure (load 16 on eight CPUs
// with PSI full 65) lands near 4.6.
const maxBudgetFactor = 6.0

// testBudget scales a quiet-host budget by the pressure the host reports at
// the moment it is called. The base stays a failure bound: a healthy path
// must finish well inside it, and on an idle host the budget is unchanged.
func testBudget(base time.Duration) time.Duration {
	loadPerCPU, psiFull := hostPressure()
	return time.Duration(float64(base) * budgetFactor(loadPerCPU, psiFull))
}

// hostPressure reads the one-minute load average normalized by CPU count and
// the "full" IO stall fraction from /proc/pressure/io. The load average
// counts runnable and uninterruptible tasks; PSI "full" counts the fraction
// of time every task was stalled on IO at once — the direct signal of fsync
// queueing, which the load average alone underreports.
func hostPressure() (loadPerCPU, psiFull float64) {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(b)); len(fields) > 0 {
			if v, err := strconv.ParseFloat(fields[0], 64); err == nil && runtime.NumCPU() > 0 {
				loadPerCPU = v / float64(runtime.NumCPU())
			}
		}
	}
	psiFull, _ = parsePressure(os.ReadFile("/proc/pressure/io"))
	return loadPerCPU, psiFull
}

// parsePressure extracts the avg60 percentage of the "full" line from the
// contents of a /proc/pressure/* file.
func parsePressure(b []byte, err error) (full float64, ok bool) {
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "full ") {
			continue
		}
		for _, field := range strings.Fields(line)[1:] {
			if v, err := strconv.ParseFloat(strings.TrimPrefix(field, "avg60="), 64); err == nil {
				return v, true
			}
		}
	}
	return 0, false
}

// budgetFactor converts the pressure readings into one multiplier. The
// anchors are the recorded failures: load 8-10 on eight CPUs stretched one
// chain past 10 s (needs about 2x), and PSI full 50-65 percent stretched a
// 16 s chain past 60 s (needs about 4x). Each CPU-load above one adds its
// excess; every 25 percent of full IO stall adds one more.
func budgetFactor(loadPerCPU, psiFull float64) float64 {
	factor := 1.0
	if excess := loadPerCPU - 1; excess > 0 {
		factor += excess
	}
	if stalls := psiFull / 25; stalls > 0 {
		factor += stalls
	}
	if factor > maxBudgetFactor {
		return maxBudgetFactor
	}
	return factor
}

func TestBudgetFactorScalesWithRecordedHostPressure(t *testing.T) {
	cases := []struct {
		name        string
		loadPerCPU  float64
		psiFull     float64
		wantAtLeast float64
		wantAtMost  float64
	}{
		// An idle host never changes a budget.
		{"idle", 0.4, 0, 1, 1},
		{"busy cpus only", 1.25, 0, 1.25, 1.25},
		// Load 11-15 on eight CPUs with PSI full 50-65 percent is the
		// recorded pressure that stretched a 16 s chain past 60 s.
		{"recorded pressure", 1.7, 57, 3.9, 4.4},
		// The worst host on record still fails tests inside the cap.
		{"capped", 4, 100, maxBudgetFactor, maxBudgetFactor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := budgetFactor(tc.loadPerCPU, tc.psiFull)
			if got < tc.wantAtLeast || got > tc.wantAtMost {
				t.Fatalf("budgetFactor(%v, %v) = %v, want within [%v, %v]",
					tc.loadPerCPU, tc.psiFull, got, tc.wantAtLeast, tc.wantAtMost)
			}
		})
	}
	if budgetFactor(1, 0) != 1 {
		t.Fatal("a fully loaded but unstalled host must not shrink budgets")
	}
	if testBudget(30*time.Second) < 30*time.Second {
		t.Fatal("testBudget must never return less than its base")
	}
}

func TestParsePressureReadsFullAvg60(t *testing.T) {
	sample := "some avg10=0.00 avg60=2.15 avg300=10.82 total=40913727722\n" +
		"full avg10=0.00 avg60=2.05 avg300=10.20 total=38090269274\n"
	v, ok := parsePressure([]byte(sample), nil)
	if !ok || v != 2.05 {
		t.Fatalf("parsePressure = %v, %v; want 2.05, true", v, ok)
	}
	if _, ok = parsePressure(nil, os.ErrNotExist); ok {
		t.Fatal("a missing pressure file must read as absent, not zero-full")
	}
	if _, ok = parsePressure([]byte("some avg10=1.00 avg60=1.00 avg300=1.00 total=1\n"), nil); ok {
		t.Fatal("a file without a full line must read as absent")
	}
}
