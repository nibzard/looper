package looper

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// useMemoryFiles points the memory bound at fixture files for one test and
// restores the host paths afterwards.
func useMemoryFiles(t *testing.T, f memoryFiles) {
	t.Helper()
	previous := hostMemoryFiles
	hostMemoryFiles = f
	t.Cleanup(func() { hostMemoryFiles = previous })
}

// fixtureMeminfo writes one meminfo file reporting the given MiB of available
// memory and returns its path.
func fixtureMeminfo(t *testing.T, mib int64) string {
	t.Helper()
	kb := mib * 1024
	path := filepath.Join(t.TempDir(), "meminfo")
	body := fmt.Sprintf("MemTotal:       %d kB\nMemFree:         %d kB\nMemAvailable:    %d kB\n", kb, kb, kb)
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// fixtureCgroup writes one cgroup hierarchy with the given limit, usage, and
// inactive file cache values, all verbatim file contents, and returns its
// (limit, usage, stat) paths.
func fixtureCgroup(t *testing.T, limit, usage, cache string) [3]string {
	t.Helper()
	dir := t.TempDir()
	paths := [3]string{
		filepath.Join(dir, "memory.max"),
		filepath.Join(dir, "memory.current"),
		filepath.Join(dir, "memory.stat"),
	}
	contents := []string{limit + "\n", usage + "\n", "anon 1048576\ninactive_file " + cache + "\n"}
	for i, p := range paths {
		if err := os.WriteFile(p, []byte(contents[i]), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

const gib = int64(1) << 30

// A container must bound the worker count even when the host reports ample
// memory, because /proc/meminfo shows the host inside a container.
func TestAvailableMemoryReturnsMinimumOfBounds(t *testing.T) {
	host := fixtureMeminfo(t, 64*1024)
	first := fixtureCgroup(t, "4294967296", "1073741824", "0") // 4 GiB limit, 1 GiB used
	second := fixtureCgroup(t, "2147483648", "536870912", "0") // 2 GiB limit, 512 MiB used
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{first, second}})
	if got := availableMemory(); got != 3*gib/2 {
		t.Fatalf("availableMemory must take the smallest bound, got %d bytes", got)
	}
	// The host bound wins only when every container bound is larger.
	host = fixtureMeminfo(t, 2*1024)
	first = fixtureCgroup(t, "4294967296", "1073741824", "0")
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{first}})
	if got := availableMemory(); got != 2*gib {
		t.Fatalf("host bound not used when it is smallest: %d", got)
	}
}

// Unlimited and unreadable bounds are ignored rather than reported as zero
// headroom, so an unlimited container keeps the host bound.
func TestAvailableMemoryIgnoresUnlimitedAndUnreadableBounds(t *testing.T) {
	host := fixtureMeminfo(t, 32*1024)
	// "max" is the cgroup v2 spelling of no limit.
	maxLimited := fixtureCgroup(t, "max", "1073741824", "0")
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{maxLimited}})
	if got := availableMemory(); got != 32*gib {
		t.Fatalf("a \"max\" limit must not bound the host value: %d", got)
	}
	// A legacy unlimited value also counts as no bound.
	legacy := fixtureCgroup(t, "9223372036854775807", "1073741824", "0")
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{legacy}})
	if got := availableMemory(); got != 32*gib {
		t.Fatalf("a legacy unlimited limit must not bound the host value: %d", got)
	}
	// A usage file that cannot be read reports no headroom at all.
	broken := fixtureCgroup(t, "4294967296", "1073741824", "0")
	os.Remove(broken[1])
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{broken}})
	if got := availableMemory(); got != 32*gib {
		t.Fatalf("an unreadable usage file must not bound the host value: %d", got)
	}
	// No readable bound at all reports zero, which callers treat as unknown.
	missing := filepath.Join(t.TempDir(), "meminfo")
	useMemoryFiles(t, memoryFiles{meminfo: missing, cgroups: [][3]string{
		{filepath.Join(filepath.Dir(missing), "a"), filepath.Join(filepath.Dir(missing), "b"), filepath.Join(filepath.Dir(missing), "c")},
	}})
	if got := availableMemory(); got != 0 {
		t.Fatalf("no readable bound must report zero, got %d", got)
	}
	// A meminfo without a MemAvailable line reports no host bound either.
	empty := filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(empty, []byte("MemTotal: 1 kB\n"), 0644); err != nil {
		t.Fatal(err)
	}
	useMemoryFiles(t, memoryFiles{meminfo: empty})
	if got := availableMemory(); got != 0 {
		t.Fatalf("a meminfo without MemAvailable must report zero, got %d", got)
	}
}

// jobs=auto must follow the container bound: the host reports ample memory
// while the cgroup allows only a few workers.
func TestJobsAutoClampsToContainerMemory(t *testing.T) {
	clearSettingEnv(t)
	auto := max(1, min(4, runtime.NumCPU()/2))
	if auto < 2 {
		t.Skip("a host with one effective CPU cannot discriminate the container clamp")
	}
	host := fixtureMeminfo(t, 64*1024)
	container := fixtureCgroup(t, "4294967296", "1073741824", "0") // 3 GiB headroom
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{container}})
	c, err := parseAt(t, "--jobs", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !c.JobsAuto || c.Jobs != 1 {
		t.Fatalf("jobs=auto must clamp to the container bound of one worker: jobs=%d auto=%t", c.Jobs, c.JobsAuto)
	}
	// An unlimited container keeps the CPU-derived count.
	unlimited := fixtureCgroup(t, "max", "1073741824", "0")
	useMemoryFiles(t, memoryFiles{meminfo: host, cgroups: [][3]string{unlimited}})
	c, err = parseAt(t, "--jobs", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !c.JobsAuto || c.Jobs != auto {
		t.Fatalf("an unlimited container must keep the CPU count %d: jobs=%d", auto, c.Jobs)
	}
	// No readable bound at all also keeps the CPU count.
	useMemoryFiles(t, memoryFiles{meminfo: filepath.Join(t.TempDir(), "meminfo")})
	c, err = parseAt(t, "--jobs", "auto")
	if err != nil {
		t.Fatal(err)
	}
	if !c.JobsAuto || c.Jobs != auto {
		t.Fatalf("an unknown memory bound must keep the CPU count %d: jobs=%d", auto, c.Jobs)
	}
}

// throttleEngine builds a controller over three independent tasks whose worker
// stubs stay running, so the test can count how many dispatch at once.
func throttleEngine(t *testing.T, jobs int) *Engine {
	t.Helper()
	bin, _ := fakeAgent(t, "")
	t.Setenv("LOOPER_FAKE_SLEEP", "30")
	c := repoConfig(t, "run", "--jobs", fmt.Sprint(jobs))
	c.CodexBin, c.ClaudeBin = bin, bin
	todo := Backlog{Version: 1, Sources: []string{}, Goal: "existing goal", Tasks: []Task{
		{ID: "T001", Title: "one", Priority: 1, Status: "todo"},
		{ID: "T002", Title: "two", Priority: 1, Status: "todo"},
		{ID: "T003", Title: "three", Priority: 1, Status: "todo"},
	}}
	if err := writeJSON(c.Todo, todo); err != nil {
		t.Fatal(err)
	}
	e, lock, err := NewEngine(c, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.initialize(context.Background()); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Store.Close(); lock.Close() })
	e.State.Plan = &Plan{Verify: "true", Tasks: []Scope{
		{TaskID: "T001", Paths: []string{"a"}},
		{TaskID: "T002", Paths: []string{"b"}},
		{TaskID: "T003", Paths: []string{"c"}},
	}}
	contracts := []string{}
	for _, task := range e.State.Backlog.Tasks {
		contracts = append(contracts, task.Contract())
	}
	e.State.PlanSignature = hashJSON(struct {
		Contracts     []string
		Setup, Verify string
	}{contracts, e.Config.Setup, e.Config.Verify})
	if err := e.save(); err != nil {
		t.Fatal(err)
	}
	return e
}

func runningAttempts(e *Engine) int {
	n := 0
	for _, a := range e.State.Attempts {
		if a.Phase == "running" {
			n++
		}
	}
	return n
}

// The dispatch throttle drops capacity to one worker while available memory
// stays under 1 GiB, independent of the configured job count.
func TestDispatchThrottleLimitsWorkersUnderLowMemory(t *testing.T) {
	clearSettingEnv(t)
	low := fixtureMeminfo(t, 400)
	useMemoryFiles(t, memoryFiles{meminfo: low})
	e := throttleEngine(t, 3)
	if e.capacity != 3 {
		t.Fatalf("fixture must allow three workers, capacity %d", e.capacity)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 130 || err != context.DeadlineExceeded {
		t.Fatalf("engine exited %d (%v); want a live loop cancelled at the deadline", code, err)
	}
	if got := runningAttempts(e); got != 1 {
		t.Fatalf("low memory must hold dispatch at one worker, got %d running", got)
	}
	var started []string
	for _, ev := range storedEvents(t, e) {
		if ev.Kind == "start" && strings.HasPrefix(ev.Message, "T") {
			started = append(started, strings.Fields(ev.Message)[0])
		}
	}
	if len(started) != 1 {
		t.Fatalf("low memory must start one worker, started %v", started)
	}
}

// With ample memory the same fixture dispatches the configured job count, so
// the throttle test above measures the throttle and not the fixture.
func TestDispatchAllowsConfiguredJobsWithAmpleMemory(t *testing.T) {
	clearSettingEnv(t)
	host := fixtureMeminfo(t, 64*1024)
	useMemoryFiles(t, memoryFiles{meminfo: host})
	e := throttleEngine(t, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, err := e.Run(ctx)
	if code != 130 || err != context.DeadlineExceeded {
		t.Fatalf("engine exited %d (%v); want a live loop cancelled at the deadline", code, err)
	}
	if got := runningAttempts(e); got != 2 {
		t.Fatalf("ample memory must allow two workers, got %d running", got)
	}
}
