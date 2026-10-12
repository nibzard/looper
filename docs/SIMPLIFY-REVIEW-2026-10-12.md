# Simplification review 2026-10-12

Method: a 32-agent review workflow. Seven finders covered the subsystems and the
repo level. They produced 52 findings. After dedup, the top 24 went through
adversarial verification: each verifier tried to refute the finding by grepping
every usage, checking docs and tags, and in several cases applying the change in
a scratch copy and running the full test suite. 17 findings survived, worth
roughly 140-160 lines removed (raw sum of estimates: 166).

Baseline before the review: `go build ./...`, `go vet ./...`, and `go test ./...`
all passed.

Caveat: the working tree changed during the review. A statistics and pricing
subsystem (about 1,300 lines) landed while the workflow ran. Apply findings by
their text anchors, not by line numbers. Re-run the baseline before applying
anything.

## Confirmed findings (17)

### 1. Backlog counting loop copied in three places

- Location: `internal/looper/view.go:20-32`
- Category: duplication | Est. savings: ~19 lines | Risk: low

Detail: The same loop (skip Marker tasks, count total/done/blocked) appears at view.go:20-32 (snapshot), style.go:332-344 (StatusBoard), and cli.go:322-337 (inspect status). The cli.go copy also prints ListStatus rows inside the counting loop.

Proposed change: Add one helper in view.go: func backlogCounts(tasks []Task) (done, blocked, total int) with the switch-form loop. Replace the loops in snapshot and StatusBoard with done, blocked, total := backlogCounts(e.State.Backlog.Tasks) and backlogCounts(s.Backlog.Tasks). In cli.go inspect, call the helper and keep a separate small loop for ListStatus rows that skips Marker tasks, so listed rows stay identical. 42 lines become about 23.

Verification: Verified all three loops: view.go:20-32 and style.go:332-344 are byte-identical switch-form counting loops over Backlog.Tasks skipping Marker() tasks; cli.go:322-337 is a semantically identical if-form loop (Status is a single string, so the cases are mutually exclusive) that also prints ListStatus rows. All three files are package looper, Tasks is []Task, and Marker() is a value method, so backlogCounts(tasks []Task) (done, blocked, total int) type-checks at every site; the name is unused elsewhere. Behavior is preserved: the counting loops have no output or side effects, and a separate cli.go loop that skips Marker() tasks with the same ListStatus condition prints identical rows in identical order when kept before the summary Fprintf. The snapshot e.State dereference point is unchanged. No locking, goroutine, tag, reflection, or build-tag surface is touched; existing tests cover the behavior through public entry points only. Net removal of about 19 duplicated lines with one source of truth for backlog counting.

### 2. changedFields repeats nine copy-pasted diff blocks; collapse to one table loop

- Location: `internal/looper/settings.go:786-825`
- Category: duplication | Est. savings: ~18 lines | Risk: low

Detail: changedFields diffs nine fields with nine structurally identical if-blocks, two of which (jobs, review_every) re-implement the string-only `set` closure inline with their own json.Marshal calls. A single table of {key, before, after} triples driven by interface comparison covers all nine. Verified: TestChangedFieldsDiff and the full settings-protocol tests pass unchanged.

Proposed change: Replace the body with one loop over []struct{key string; before, after any}{{"agent", before.Agent, after.Agent}, ...{"review_every", before.ReviewEvery, after.ReviewEvery}}; skip when f.before == f.after, otherwise fields[f.key] = json.Marshal(f.after). Interface != comparison is safe here because every entry holds a comparable string or int.

Verification: Applied the exact proposed rewrite to internal/looper/settings.go and verified: go vet clean, TestChangedFieldsDiff and the full internal/looper plus internal/ui suites pass unchanged, then reverted. All nine ui.Settings fields (internal/ui/forms.go:16-26) are string or int, so the any-typed table comparison f.before == f.after is exact value equality with no panic path; each row compares same-typed operands. The function is pure (no locking, no side effects, no error text), json.Marshal through any holding string/int emits identical bytes, and the error-swallowing branch is identical and unreachable. Only three callers exist (runConfigOnline, submitSettingsFromForm, the test); nothing in scripts, docs, cmd, reflection, or build tags depends on the block form. The loop also matches the table idiom already used by patchProfile in the same file. Two minor notes that do not refute: actual savings is 13-15 lines (40 to 25), not the claimed 18; and gofmt wants one extra space so the struct fields align (key string under before, after any) — run gofmt -w when applying.

### 3. signalController has no callers

- Location: `internal/looper/controller.go:113-127`
- Category: dead-code | Est. savings: ~16 lines | Risk: low

Detail: signalController (controller.go:113-127) is never called anywhere in the repo, including tests, cmd/looper, internal/ui, and adapters. `looper stop` now works by writing the drain file (cli.go:123-133) and the daemon child installs its own signal handlers (installControllerSignals, daemon.go:205). The function's comment also no longer matches its body (it kills the process, not a process group). The syscall import at controller.go:9 is used only by this function.

Proposed change: Delete signalController and drop the now-unused "syscall" import from controller.go.

Verification: signalController (controller.go:118-130, comment at 116-117) has zero callers: repo-wide grep finds only the definition and its comment, in production code, tests, cmd/, docs/, and scripts; git grep at HEAD shows the same, and the only commit touching the identifier (52beac1) introduced it. The syscall import in controller.go is used solely by this function (lines 118, 126), so dropping it with the deletion is required for compilation. The finding's context claims verify: `looper stop` writes a drain file in cli.go, the daemon child installs its own handlers via installControllerSignals (daemon.go:205, called at daemon.go:181 and cli.go), and the function comment ("process group") is stale because the body kills only the recorded PID, not -PID. Empirical check: deleting the function plus import, then `go build ./...` and `go vet ./internal/looper/` both pass; the working-tree file was restored byte-exact afterward. Deleting an uncalled, unexported standalone function is behavior-preserving. Only nit: the cited range 113-127 is off by a few lines; the real span is 116-130.

### 4. workRetained duplicates the retained() predicate from engine.go

- Location: `internal/looper/imports.go:332-346`
- Category: duplication | Est. savings: ~14 lines | Risk: low

Detail: (e *Engine) workRetained() is line-for-line the same logic as retained(s *State) at internal/looper/engine.go:223-235: publication pending, or any attempt whose phase is not done/blocked. workRetained has exactly one production caller (imports.go:296) and no test callers (grep found none), so the copy can simply delegate or be inlined.

Proposed change: Delete workRetained (lines 332-346) and change the single call site at imports.go:296 to `if retained(e.State) {`. Pure state predicate; no events or goroutines involved, so the owner-goroutine rule is untouched.

Verification: workRetained (imports.go:334-346) is character-for-character identical to retained(s *State) (engine.go:223-235): publication pending, else any attempt phase not in {done, blocked}. Whole-repo grep finds workRetained only at its definition, its comment, and the single call site imports.go:296 — no test callers, no references in cmd/, scripts/, skills/, or docs/, no interface declares it, and no build-tag or platform variants. retained is a package-private function in the same package already called from engine.go:299 and migration.go:44, so `retained(e.State)` compiles and is behavior-identical: both are pure state predicates with no events, goroutines, or locking (owner-goroutine rule untouched), and the nil-State edge case panics identically in both. Empirically verified in a scratch copy of the repo: applied the exact proposed edit, then go build ./..., go vet ./internal/looper/, and the full go test ./... -count=1 all passed. docs/ARCHITECTURE.md and docs/LOOP-CORRECTNESS-PLAN.md never mention workRetained, so no documented intent requires the duplicate.

### 5. MarshalJSON assembles JSON by hand with unreachable error branches

- Location: `internal/looper/profile.go:193-240`
- Category: needless-defense | Est. savings: ~10 lines | Risk: low

Detail: MarshalJSON marshals each key and value separately and checks four json.Marshal errors that cannot fire: the ordered table holds only string and int values (Validate already ran), and marshaling a string key never fails. Marshaling one anonymous struct produces the same bytes (struct fields serialize in declaration order) and needs one legitimate error check. Verified byte-identical output, including unknown keys, sorted, and re-indented by MarshalIndent in SaveProfile.

Proposed change: Delete the ordered slice; marshal an anonymous struct with json tags version/agent/review_agent/jobs/model/claude_model/effort/verify_command/setup_command/review_every populated with the same values. If len(p.Unknown) == 0 return it; otherwise strip the trailing '}', append the sorted unknown keys (single ignored-error json.Marshal per key), and close the brace.

Correction from the verifier: As proposed, with two notes for the applier: the code has three unreachable json.Marshal error checks (lines 216, 220, 233), not four; and the measured saving is 8 LOC (48 to 40 lines). Keep the value receiver func (p Profile) MarshalJSON so *Profile still satisfies json.Marshaler, marshal only the anonymous struct (never p itself) to avoid recursion, and keep the sort.Strings import for the unknown-key loop.

Verification: Every refutation angle failed. (1) The dead branches are genuinely unreachable: the ordered table holds only Go int and string values, and json.Marshal never errors on int or string (invalid UTF-8 is replaced with U+FFFD, not an error), so the checks at profile.go:216-217, 220-221, and 233-234 cannot fire. (2) Byte-identity is proven, not assumed: a side-by-side harness over 9 adversarial profiles (HTML chars, invalid UTF-8, NUL, nil vs empty Unknown, empty-string key, unknown key shadowing a known key, raw values with embedded whitespace) produced identical bytes for direct MarshalJSON, json.Marshal, and json.MarshalIndent — the actual path in SaveProfile/saveProfileExclusive (profile.go:155, 247) — and identical decoded maps. Struct fields serialize in declaration order, matching the ordered table and the schema in docs/UI-PLAN.md. The trailing-'}' strip is safe because the 10-field struct encoding always ends in '}'. (3) Repo-wide grep found no other consumer of the byte form: no reflection, platform-suffixed files, scripts, or golden-byte tests; tests round-trip semantically. The value receiver keeps both Profile and *Profile satisfying json.Marshaler, and the anonymous struct avoids recursion. (4) The exact proposed change, applied in an isolated copy of the working tree, is gofmt-clean, builds, and passes the full go test ./... suite. Only trivial inaccuracies: the finding counts four dead json.Marshal error checks (there are three) and claims 10 LOC saved (measured 8, from 48 to 40 lines in the function). Neither affects correctness of the change. The real tree transiently failed to build from an unrelated concurrent pricing.go work-in-progress; verification was done in a copy and the original file was restored byte-identical.

### 6. Width closures re-inline the type assertion that fileOf already provides

- Location: `internal/looper/cli.go:252-257`
- Category: duplication | Est. savings: ~10 lines | Risk: low

Detail: cli.go:252-257 (doctor) and engine.go:134-141 (NewEngine) both write `if f, ok := out.(*os.File); ok { return terminalWidth(f) }; return 0`, which is exactly `terminalWidth(fileOf(w))` given fileOf (style.go:249-252) returns nil for non-file writers and terminalWidth(nil) returns 0. cli.go:340 already uses the one-line form, proving the idiom.

Proposed change: Replace both multi-line closures with `func() int { return terminalWidth(fileOf(out)) }`. As a bonus, writerTTY (style.go:35-38) can become `return isTTY(fileOf(w))`.

Verification: Could not refute. cli.go:252-257 and engine.go:134-141 duplicate exactly what terminalWidth(fileOf(out)) computes: fileOf (style.go:249-252) returns nil for non-file writers and terminalWidth has an explicit nil-to-0 guard (process_unix.go:184-194), so results are identical for real files, typed-nil *os.File, other writers, and nil interfaces. The one-line idiom is already used twice in the same file (cli.go:340, 357). The writerTTY bonus is likewise equivalent because isTTY(nil) returns false (style.go:26-31). No reflection, tags, scripts, docs, or build-constraint couplings exist; the linux/darwin constraint from process_unix.go was already binding since cli.go calls terminalWidth unconditionally. I applied the change (both closures plus writerTTY), ran go build ./... and go test ./internal/looper/ — build clean, all tests pass — then restored the tree byte-exact. The change is behavior-preserving and a genuine deduplication of about 10 lines.

### 7. Same sync-then-inspect block repeated in three key handlers

- Location: `internal/ui/workspace.go:289-295`
- Category: duplication | Est. savings: ~10 lines | Risk: low

Detail: The block "m.syncTasks(); if width >= 110 { if t := m.workspaceTask(); t != nil { return m.loadTaskDetail(t.ID) } }; return nil" appears three times: dashboard.go 252-258 (search esc/enter), workspace.go 289-295 (tab), and workspace.go 333-338 (filter key f). The gates are equivalent in all three sites: searching is only set true in ModeTasks, and the tab site checks m.mode == ModeTasks explicitly.

Proposed change: Add one helper on *model: func (m *model) refreshTasks() tea.Cmd { m.syncTasks(); if m.mode == ModeTasks && m.width >= 110 { if t := m.workspaceTask(); t != nil { return m.loadTaskDetail(t.ID) } }; return nil }. Replace the three blocks with return m, m.refreshTasks() (search site) and return true, m, m.refreshTasks() (tab and f sites). Do not touch the "u" case, which deliberately loads the detail without the width gate.

Correction from the verifier: Same change as proposed; apply by text anchors, not line numbers, because the working tree changed mid-review (a ModeStatistics tab stop was added). Current anchors: dashboard.go 278-284 (search esc/enter fall-through), workspace.go 293-299 (tab), workspace.go 337-343 (filter key f). Add the refreshTasks helper near loadTaskDetail in workspace.go, replace the three blocks with "return m, m.refreshTasks()" (search site) and "return true, m, m.refreshTasks()" (tab and f sites), and leave the "u" case untouched.

Verification: The three sync-then-inspect blocks exist verbatim and are gated equivalently. searching=true is set only in the "/" handler immediately after m.mode = ModeTasks, and while searching all keys are intercepted in keys() before any mode-changing handler, so the helper's added m.mode == ModeTasks check is a tautology at the search site; the tab site's gate is textually identical to the helper's; the f site is already inside if m.mode == ModeTasks. The "u" case (no width gate) and the tick/nav variants (different conditions) are correctly excluded. return m, m.refreshTasks() preserves mutation-before-copy semantics (verified empirically with a Go program; the existing code already uses the same shape in return true, m, m.loadTaskDetail(t.ID)). No refreshTasks symbol exists yet, and no tags, reflection, interfaces, scripts, or docs reference these blocks. I applied the exact proposed change to a scratch copy of the repo: go build ./..., go vet ./internal/ui/, and the full go test ./... all pass. Only caveat: the finder's line numbers have drifted because a statistics feature was concurrently added to the working tree mid-review (blocks now at dashboard.go 278-284, workspace.go 293-299, workspace.go 337-343), and net LOC saved is closer to 6-9 than 10. Neither refutes the finding.

### 8. runDashboardController and runAttach rebuild the same ui.Controls closures

- Location: `internal/looper/dashboard.go:74-96`
- Category: duplication | Est. savings: ~10 lines | Risk: medium

Detail: Lines 74-81 and 215-222 assign the same six closures (Inspect, WorkerLog, Log, Poll, Events, Diff) built from the same dir/info/observer, and lines 96 and 225 assign a byte-identical Requeue closure. Lines 92-95 also introduce a pointless `drain` intermediate variable before controls.Drain.

Proposed change: Add `func observerControls(dir string, info *ControllerInfo, o *observer) ui.Controls` returning the six common fields, and call it in both places; assign controls.Drain directly as a func literal in runDashboardController. Keep Snapshot/Done/Cancel and the ReadOnly/withControls differences at their call sites so no behavior moves.

Verification: Verified in internal/looper/dashboard.go: the two ui.Controls literals (lines 74-92 in runDashboardController, 216-225 in runAttach) share byte-identical closures for Statistics, Inspect, WorkerLog, Log, Poll, Events, and Diff, all built from the same dir/info/observer, and the Requeue assignments (lines 97 and 227) are byte-identical. The proposed helper signature works at both sites: e.controller is *ControllerInfo (engine.go:33) and readController returns *ControllerInfo (controller.go:73), with observer.inspect/observer.view defined on *observer (dashboard_data.go:129, 24). The drain variable (lines 93-95) has one reference, so inlining it is behavior-identical. Genuine differences (Snapshot channel, Done capturing the local done channel, Cancel, ReadOnly, the session-validating Drain in runAttach, and the two AddPlan bodies) stay at call sites per the proposal, so no observable behavior changes: closure capture through helper parameters is equivalent because dir, info, and observer are never reassigned, and no reflection, tags, scripts, or docs reference these fields. Two minor inaccuracies that do not affect correctness: the shared set is seven fields, not six (Statistics was omitted from the list), and the Requeue lines are 97/227 rather than 96/225. Net savings is roughly 6-10 lines, close to the claimed 10.

### 9. LoadProfile repeats the decodeString block seven times instead of looping

- Location: `internal/looper/profile.go:82-102`
- Category: duplication | Est. savings: ~9 lines | Risk: low

Detail: Seven consecutive `if err := decodeString(key, &p.Field); err != nil { return nil, err }` blocks differ only in key and target. settings.go:465-476 (patchProfile) already uses the table-driven form for the same field set, so the loop style is established in this package.

Proposed change: Replace lines 82-102 with one loop over []struct{ key string; into *string }{{"agent", &p.Agent}, {"review_agent", &p.ReviewAgent}, {"model", &p.Model}, {"claude_model", &p.ClaudeModel}, {"effort", &p.Effort}, {"verify_command", &p.VerifyCommand}, {"setup_command", &p.SetupCommand}} calling decodeString. Same field order, same first-error return, so error behavior is unchanged.

Verification: The seven decodeString blocks at internal/looper/profile.go:82-102 differ only in key and target pointer, and settings.go:457-476 (patchProfile) already uses the same table-driven loop for the same seven fields, so the style is established in the package. I applied the exact proposed change to a scratch copy: gofmt-clean, go build ./... passes, and both the Profile/Settings tests and the full internal/looper suite pass. Semantics are identical: same field order (agent, review_agent, model, claude_model, effort, verify_command, setup_command), same first-error return, error text produced by the unchanged decodeString closure, no side effects or locking. MarshalJSON uses its own ordered table, so nothing depends on statement shape, struct tags, or reflection; no platform-suffixed variants or script/doc references exist. Tests assert only error substrings, which are unchanged. Net savings is exactly the claimed 9 LOC (21 lines to 12). One intermittent getwd test failure observed under /tmp was an environment artifact, did not reproduce on re-run, and is unrelated to the patch.

### 10. matchingImport reimplements slices.Contains with manual flag loops

- Location: `internal/looper/imports.go:213-230`
- Category: stdlib-replacement | Est. savings: ~8 lines | Risk: low

Detail: matchingImport builds an `accepted` flag by looping over statuses (lines 214-217) and a `match` flag by looping over digests (lines 221-226). Go 1.22 provides slices.Contains and slices.ContainsFunc for exactly these patterns. The `match` loop also skips no elements after finding a miss, so it is a pure predicate.

Proposed change: Replace the body with: `if !slices.Contains(statuses, r.Status) || len(r.Digests) != len(digests) { continue }` and `if !slices.ContainsFunc(r.Digests, func(d string) bool { return !digests[d] }) { return r }`. Add the "slices" import.

Verification: The two flag loops in matchingImport (/home/agent/looper/internal/looper/imports.go:213-230) are pure reductions with no side effects, early exits, locking, or error output, so slices.Contains(statuses, r.Status) and !slices.ContainsFunc(r.Digests, !digests[d]) are behavior-identical, including edge cases (empty statuses, nil or duplicate digests — the len(r.Digests) != len(digests) gate is unchanged). go.mod declares go 1.22 and both slices helpers ship since Go 1.21. Both callers (imports.go:205 and requirements.go:97) keep the variadic signature, and nothing else references matchingImport. I applied the exact proposed change to a scratch repo copy: gofmt clean, go build, go vet, and the full go test ./internal/looper/ suite pass, and a 13-case differential test of the old and new bodies returned identical results on every case. The change saves about 8 lines and adds only the slices import.

### 11. Environment defaults built with map iteration plus switch; replace with data tables

- Location: `internal/looper/config.go:142-171`
- Category: other | Est. savings: ~8 lines | Risk: medium

Detail: Both startup loops iterate a map of key to pointer, then recover each default with a switch over the key (lines 142-155 for ints, 156-171 for durations). A slice of {key, into, def} triples states each binding once, keeps the int-before-duration validation order, and removes the accidental nondeterminism: today, when two environment variables are invalid at once, which error message is returned varies per run with map order; a slice makes it fixed. Single-invalid-variable behavior (what tests assert) is unchanged.

Proposed change: Replace each loop with `for _, e := range []struct{key string; into *int; def int}{{"MAX_ITERATIONS", &c.Limit, 0}, {"LOOPER_MAX_REPAIRS", &c.MaxRepairs, 6}, {"LOOPER_RATE_LIMIT_MAX", &c.RetryLimit, 12}}` (and the analogous *time.Duration table with defaults 30/60/0/600), calling integer(e.key, e.def) and assigning *e.into.

Verification: Applied the proposed table-based rewrite to a scratch copy of /home/agent/looper/internal/looper/config.go and could not refute it. The defaults in the proposed tables are correct (ints: MAX_ITERATIONS=0, LOOPER_MAX_REPAIRS=6, LOOPER_RATE_LIMIT_MAX=12; durations: POLL=30, RATE_LIMIT_DELAY=60, TIMEOUT=0, GIT_TIMEOUT=600), matching the current switches, TestParseDefaults, and README's env table. gofmt/go vet/go build are clean and the full test suite passes (go test ./..., all packages ok). The integer() helper has no other callers; no reflection, tags, build tags, or platform variants are involved; scripts only set valid values and no test asserts these error messages or sets two invalid variables, so nothing observes the old map-order randomness. Single-invalid-variable error text is byte-identical (probed directly), int-before-duration ordering and env-before-args validation precedence are preserved, and the block is startup-only code with no concurrency or event-dispatch relevance. The change states each binding once instead of twice (map entry plus switch case) and saves exactly the claimed 8 lines (504 to 496). The only behavioral delta is the one the finding discloses: which error surfaces when several variables are invalid at once becomes fixed instead of random per run — a strict narrowing of possible outputs that no test, script, or doc depends on.

### 12. digestFile is a byte-identical copy of profileDigest

- Location: `internal/looper/imports.go:404-410`
- Category: duplication | Est. savings: ~7 lines | Risk: low

Detail: digestFile(path) reads a file and returns digest(bytes), returning "" on error. profileDigest at internal/looper/profile.go:256-262 does exactly the same. digestFile has 2 production callers (imports.go:367,377) and 3 test callers (imports_test.go:135,183,192); profileDigest has 10+ production callers.

Proposed change: Delete digestFile and rename its 5 call sites to profileDigest. The three test lines are mechanical renames, not semantic rewrites; behavior is identical because the bodies are identical.

Verification: Verified: digestFile (internal/looper/imports.go:404-410) and profileDigest (internal/looper/profile.go:256-262) are plain functions in the same package with identical bodies apart from the local variable name (b vs raw): os.ReadFile(path), return "" on error, else digest(bytes), both calling the shared digest helper at types.go:169. digestFile has exactly 5 callers as claimed (imports.go:367,377; imports_test.go:135,183,192); a whole-repo search excluding .git found no other references to either identifier outside internal/looper (no cmd/, scripts/, docs/, struct-tag, reflection, build-tag, or platform-file involvement). The rename is behavior-preserving: same signature, error path, evaluation order, no locking or side effects. It is compile-safe because os remains used in imports.go (lines 35, 51, 75, 92, 148, 328) after deleting lines 404-410, and go build ./... passes. The change touches no goroutines or event dispatch. Two imprecisions in the finding do not refute it: the bodies are functionally (not literally byte-) identical due to the local variable name, and profileDigest's name/doc comment are profile-specific while the migrated production callers digest the Todo backlog file — a naming wart that should be resolved by generalizing profileDigest's doc comment in the same change, with no observable behavioral effect.

### 13. mergeUnique reimplements slices.Sort plus slices.Compact

- Location: `internal/looper/engine.go:821-832`
- Category: stdlib-replacement | Est. savings: ~6 lines | Risk: low

Detail: mergeUnique concatenates two slices, dedupes with a map, and sorts. Go 1.22's slices package does this directly: sorting the concatenation then compacting adjacent duplicates yields the identical result (a sorted, deduped merge), including the nil,nil case which produces an empty slice. The test in types_test.go:176-184 pins exactly this behavior ("a,b,c" and len 0). The "sort" import in engine.go is used only here.

Proposed change: Replace the body with `out := append(append([]string{}, old...), fresh...); slices.Sort(out); return slices.Compact(out)` and swap the "sort" import for "slices".

Verification: Could not refute. Current mergeUnique (engine.go:827-838) does first-occurrence dedup then sort; sorting the concatenation then slices.Compact yields the identical sorted, deduped result for every input including (nil, nil), where both versions return the same non-nil empty slice (Compact returns s unchanged for len<2, matching the []string{} start of the old code, so the len==0 test and any JSON nil-vs-empty behavior are unchanged). Both versions build the result from append(append([]string{}, old...), fresh...) — an expression the old code already uses — so return values alias no input in either version. Repo-wide scan: mergeUnique is used only at engine.go:782-783 and pinned by TestMergeUnique (types_test.go:176-184); no reflection, struct tags, interface satisfaction, build tags, scripts, or docs reference it; it is a pure function so the owner-goroutine/event rules are unaffected. The "sort" import is used only at line 836; "slices" is neither imported nor shadowed anywhere in the package's production files; go.mod declares go 1.22 so slices.Sort/Compact (Go 1.21+) are available. Empirically applied the exact proposed change: go build and go vet clean, TestMergeUnique and TestApplyResultAccumulates pass, full go test ./internal/looper suite passes; edits reverted and working tree restored. Body shrinks from 12 to 4 lines with a one-for-one import swap. Minor inaccuracy: the finding's line range 821-832 is off by six lines (actual 827-838 in the current dirty working tree), but the block is uniquely identified by name and the change text is exact.

### 14. shortDuration has two identical branches

- Location: `internal/looper/style.go:317-326`
- Category: dead-code | Est. savings: ~6 lines | Risk: low

Detail: The `case d < time.Hour` arm and the `default` arm both return d.Truncate(time.Minute).String(), so the switch cannot distinguish them. Only production caller is style.go:265; no test references shortDuration.

Proposed change: Collapse to: `func shortDuration(d time.Duration) string { if d < time.Minute { return d.Truncate(time.Second).String() }; return d.Truncate(time.Minute).String() }`.

Verification: The finding is accurate. At /home/agent/looper/internal/looper/style.go:317-326, the `case d < time.Hour` arm (line 322) and the `default` arm (line 324) both return the identical expression `d.Truncate(time.Minute).String()`, so the switch cannot distinguish them. A repo-wide sweep (production code, tests, cmd/, scripts/, docs/, skills/, platform-suffixed and hidden files) finds exactly one caller, style.go:265 in Presenter.Event, and no test or documentation references; the function is unexported and free of reflection, struct tags, interface satisfaction, or build-tag entanglement. The proposed collapse is behavior-preserving for every input: both versions map d < time.Minute to second-level truncation and all d >= time.Minute (including the boundaries d == time.Minute and d == time.Hour, and negative durations, which take the first branch in both) to minute-level truncation, and Duration.Truncate has no side effects. The change touches only a pure formatting helper, so the loop-correctness owner-goroutine rules are not implicated. Net effect is a real simplification: the 10-line function becomes 6 lines with identical output.

### 15. Repair-then-reactivate block duplicated in both failure paths of Run

- Location: `internal/looper/run.go:146-152`
- Category: duplication | Est. savings: ~5 lines | Risk: low

Detail: The check-failure path (run.go:146-152) and the prepare-conflict path (run.go:191-197) contain the same sequence: call e.repair with a feedback string, propagate the error as `return 1, err`, and mark the attempt active again when repair relaunched a worker. A closure defined before the main loop keeps the owner-goroutine event rule intact (both call sites are on the owner goroutine; nothing moves across goroutines).

Proposed change: Define `redispatch := func(a *Attempt, feedback string) error { if err := e.repair(ctx, a, feedback, results); err != nil { return err }; if a.Phase == "running" { active[a.ID] = true }; return nil }` before the loop and call it from both sites, replacing each six-line block with a two-line error check.

Correction from the verifier: Same change, with honest line accounting. Insert after `deferred := ""` (run.go:52), before the main loop: `redispatch := func(a *Attempt, feedback string) error { if err := e.repair(ctx, a, feedback, results); err != nil { return err }; if a.Phase == "running" { active[a.ID] = true }; return nil }` (gofmt expands this to 8 lines). Replace run.go:146-151 with `if err := redispatch(a, failureEvidence(result.Feedback)); err != nil { return 1, err }` and run.go:191-196 with `if err = redispatch(a, candidate.Feedback); err != nil { return 1, err }`, keeping both `continue` statements. Net effect: duplication removed (the repair/reactivate invariant lives in one place), but the file grows by 2 lines, not shrinks by 5 as the finding claimed.

Verification: The duplication claim is accurate: run.go:146-152 and run.go:191-197 repeat the same repair-then-reactivate sequence (call e.repair, propagate its error as `return 1, err`, set active[a.ID] when repair relaunched a worker). The proposed closure is behavior-preserving: I applied it verbatim (gofmt-expanded) to a scratch copy of the working tree and verified gofmt-clean, `go build ./...`, `go vet`, and the full internal/looper test suite passing. Identical error values, exit codes, map writes, evaluation order, and side effects; the `continue` statements remain at their own call sites (different loops). Captured variables ctx, results, and active are all declared before the loop and never reassigned in run.go, and the closure parameter `a` avoids capturing the stale loop variable. The owner-goroutine rule (engine.go:23) is intact: the closure is synchronous and both call sites are on Run's owner goroutine. No `redispatch` identifier conflicts anywhere in the repo, and tests that call e.repair directly (diagnosis_test.go) are unaffected. Only flaw found: the claimed savings_loc of 5 is inverted — the change nets +2 lines (9-line closure added, 7 lines removed), so it removes duplication but grows the file slightly; this is a metadata error, not a defect in the change itself.

### 16. mustWD is a single-use pass-through wrapper

- Location: `internal/looper/controller.go:51-57`
- Category: over-abstraction | Est. savings: ~5 lines | Risk: low

Detail: mustWD (controller.go:51-57) wraps os.Getwd, swallows the error, and has exactly one caller: the ControllerInfo literal in publishController (controller.go:39). Despite the "must" name it never panics; it just returns an empty string, so the wrapper adds a misleading name and seven lines for one field assignment.

Proposed change: Delete mustWD and inline `wd, _ := os.Getwd()` before the ControllerInfo literal in publishController, using `Root: wd`.

Verification: All claims check out. Repo-wide grep shows mustWD (controller.go:51-57) has exactly one caller — the ControllerInfo literal in publishController at controller.go:39 — with no other references in tests, platform-suffixed files, scripts, or docs. It returns "" on error rather than panicking, so the "must" name is misleading Go convention. Inlining `wd, _ := os.Getwd()` before the literal is behavior-preserving: os.Getwd returns "" with any error, so Root gets the same value; the evaluation-order shift is unobservable because identifier() (engine.go:81) only reads crypto/rand; ControllerInfo.Root is never read back (readController validates only Version, Session, PID); and the os import remains used by os.Getpid/os.Remove/os.ReadFile/os.IsNotExist. The change touches no concurrency or event dispatch, so the owner-goroutine rules in docs/ARCHITECTURE.md and LOOP-CORRECTNESS-PLAN.md are not implicated. Removing a misleadingly named single-use wrapper is a genuine simplification (net ~6 LOC saved, at least the claimed 5).

### 17. Review-agent normalization duplicated in both form runners; sessionsInput takes an unused parameter

- Location: `internal/ui/forms.go:122-165`
- Category: duplication | Est. savings: ~5 lines | Risk: low

Detail: RunFirstRunForm (97-100 and 112-115) and RunEditorForm (123-126 and 152-155) repeat the same decode block ("review := sameAsAgent; if s.ReviewAgent != \"\" { review = s.ReviewAgent }") and encode block ("s.ReviewAgent = \"\"; if review != sameAsAgent { s.ReviewAgent = review }"). Also sessionsInput(s *Settings, value *string) at line 66 never reads s.

Proposed change: Add func decodeReview(agent string) string { if agent != "" { return agent }; return sameAsAgent } and func applyReview(s *Settings, selection string). Replace the four inline blocks with single calls. Change the signature to sessionsInput(value *string) and update the call at line 136 to sessionsInput(&sessions).

Verification: Verified in /home/agent/looper/internal/ui/forms.go: the decode blocks at lines 97-100 and 123-126 and the encode blocks at lines 112-115 and 152-155 are character-identical duplicates of the "empty ReviewAgent = sameAsAgent" invariant, and sessionsInput at line 66 never reads its s parameter (body uses only value/autoSessions/strconv/fmt). The proposed change is behavior-preserving: helpers are exact equivalents, decode still precedes form construction, encode still follows the form.Run error check and precedes the sessions/frequency parsing in RunEditorForm; no locking, side effects, error text, or output formatting involved. sessionsInput is unexported with a single call site (line 136, same file); repo-wide grep over Go, tests, scripts, and docs found no other references, no reflection, struct tags, interface satisfaction, build tags, or platform-suffixed variants, and no name collisions for decodeReview/applyReview in package ui. Callers of RunFirstRunForm/RunEditorForm in internal/looper pass *ui.Settings and are unaffected. Caveats that do not refute: savings_loc 5 is overstated (the swap is roughly line-neutral: 16 inline lines become 12 helper lines plus 4 call lines; the real value is single-source-of-truth plus the dead-parameter removal), and the sibling reviewAgentField(s *Settings, selection *string) also never reads s, a similar cleanup outside this finding's scope.

## Refuted findings (7)

These died under verification. The reasons matter: they show what a wrong
simplification looks like in this repo.

- **Legacy Python-controller migration path is untested and unreachable for current users** (`internal/looper/migration.go`)
  Reason: The finding's factual premises fail on three counts. (1) "Untested" is false: the finder only grepped *_test.go, but scripts/runtime-test.py has two dedicated tests for this path — test_python_blocked_partial_work_is_migrated (line 547) and test_python_publication_migration (line 592) — which write logs/parallel/state.json, delete the Go state.db, and assert the loop imports the journal and resumes retained work to completion (the second also asserts the original journal is preserved). The Makefile `test` and `race` targets run `python3 scripts/runtime-test.py`, so the finder's "full test suite passes" claim covers only `go test ./...`, not the suite this repo defines. (2) "Unreachable for current users" is false: docs/migration.md lines 6-11 document the import as a supported path — "The Go controller imports an unfinished Python parallel journal from the same project directory... and resumes retained work" — so any project carrying a Python-era journal is a supported user of this code. (3) The change is not behavior-preserving by the finder's own admission (fresh start instead of import; orphaned legacy worker processes not reaped), which alone fails the confirmation bar. The project has also already decided this deliberately: docs/research/PRACTICES-REPORT-2026-10-11.md line 299 lists "Legacy migration | migration.go (120 lines) | One-shot import of the Python controller's state | Time-box to a removal release" and line 367 says to set removal dates for it — i.e., removal belongs to a planned removal release with deprecation notice, not a simplification pass. Minor nit: the call block is at internal/looper/engine.go:278-283 in the current tree, not 272-277.

- **Footer strings and viewport height formula duplicated across five screen functions** (`internal/ui/dashboard.go`)
  Reason: The duplication is real and mostly as cited: the esc-variant footer literal occurs at internal/ui/dashboard.go:1079, 1193, and 1210 (three times, not four as claimed); the ModeTail variant at 1024/1212; the diff variant at 1033/1215; the -3 height formula at 1025, 1034, 1080, 1214, 1216; and workspace.go activityScreen (962-974) repeats workspaceViewportScreen (944-948) scaffolding. The activityScreen rewrite is exactly behavior-preserving (workspaceFooter is deterministic; viewport Width/Height are plain field assignments), and replacing literals in tailScreen/diffScreen/helpView/logScreen is safe because renderView (dashboard.go:668-683) dispatches each screen only under its matching mode. However, the scrollFooter() change as specified is not behavior-preserving: current sizeViewports sizes m.tailView with the esc-variant footer for every mode except ModeTail (line 1211 tests only ModeTail), while the proposed ModeDiff branch sizes it with the longer diff footer. At supported narrow widths (the code has m.width < 50 branches and tests at width 30) "diff <id> · [j/k] Scroll · [esc] Back" (33+len(ID) columns) wraps taller than the 37-column esc set, so the stored tailView.Height shrinks by a line in ModeDiff; pressing "t" then runs GotoBottom (dashboard.go:356) after Update's top-of-message sizeViewports (line 141) ran under ModeDiff, anchoring YOffset one line too deep for the rendered frame (bubbles v0.20.0 viewport.go:139-140, 235). This contradicts the finding's own "all strings and formulas stay byte-identical" claim. Additionally, "collapse the sizeViewports if/else (1210-1218)" covers the ModeLog override at 1217-1219 (logHeader with -2), which scrollFooter() cannot express; removing it would break log-mode scroll math used by refreshLog's AtBottom/GotoBottom (lines 1165-1170).

- **daemonTail has no callers** (`internal/looper/daemon.go`)
  Reason: The dead-code claim itself is accurate — daemonTail has no callers in the working tree or in HEAD (full-repo case-insensitive grep matches only the definition; it is an unexported package-level func, so no interface, reflection, struct-tag, build-tag, or platform-suffix usage is possible, and no test references it). But the proposed change as stated is not behavior-preserving: daemon.go's `strings` import is used only inside daemonTail (lines 226 and 230), so deleting only the function leaves an unused import and the package fails to compile. The change is subtly wrong as written.

- **Store.Events has no production callers; all reads go through RecentEvents** (`internal/looper/store.go`)
  Reason: The change as proposed does not compile. The finding claims exactly five test call sites and that removal plus those edits passes the full suite. The live working tree has six call sites: it missed internal/looper/timing_test.go:269 (s.Events(0) on a *Store from OpenStore). I empirically deleted Store.Events and ran go test -c: the package fails to compile at all six sites, including timing_test.go:269, which the proposed change never edits. So the finder's verification claim is false for the current tree (timing_test.go is a new untracked file; the tree is being edited concurrently, and a schema line even appeared in store.go during my check). The underlying observation is otherwise accurate: Store.Events (store.go:95) has no production callers, no interface requires it (ui Controls.Events is an unrelated two-arg struct function field wired to RecentEvents via observer.events), and no script or doc references it. RecentEvents(0, 1<<30) is query-identical to Events(0) (WHERE id>0 ORDER BY id plus a LIMIT of 2^30 rows), so a corrected version is behavior-preserving. But the finding as written is wrong, its savings of 8 lines costs edits in six test files, and it carries a magic 1<<30 sentinel; refuted per the rule that a subtly wrong change gets a correction rather than confirmation.

- **Manual key collection plus sortStrings wrapper duplicates stdlib** (`internal/looper/dashboard.go`)
  Reason: The premise "Go 1.22 provides maps.Keys returning a slice" is false. Verified empirically: compiling `keys := maps.Keys(m); slices.Sort(keys)` against go 1.22 (go.mod `go 1.22`, toolchain go1.22.2, which the project deliberately pins for Charm deps) fails with `undefined: maps.Keys`. The Go 1.22 stdlib maps package has only Clone, Copy, DeleteFunc, Equal, EqualFunc; maps.Keys was added in Go 1.23 and returns iter.Seq[K], so even then slices.Sort(maps.Keys(m)) would not compile — the slice-returning Keys lives in golang.org/x/exp/maps, not stdlib. The exact change as written therefore breaks the build, and the optional settings.go:767-771 treatment fails the same way. The surrounding observations are correct (sortStrings at imports.go:519 is a pass-through sort.Strings wrapper with exactly one caller at dashboard.go:185; sort appears nowhere else in imports.go; no other references repo-wide), but the proposed mechanism is wrong, so the finding as stated cannot be confirmed.

- **WindowSizeMsg re-sets help and tail content that the screens re-set on render** (`internal/ui/dashboard.go`)
  Reason: The premise fails on two independent grounds. (1) For ModeTail in workspace mode with a paused tail, View does NOT re-set the content: tailScreen (internal/ui/dashboard.go:1057-1060) delegates to activityScreen (internal/ui/workspace.go:969-972), which calls tailView.SetContent only when !m.workspace.tailPaused, and the 1-second tick refresh (internal/ui/dashboard.go:233-238) is gated the same way. When the user scrolls up in the activity view (pauseActivity) and then resizes the terminal, the WindowSizeMsg branch at dashboard.go:159-161 is the only code that re-wraps the tail at the new width. The bubbles v0.20.0 viewport does no soft wrap (SetContent only splits on newline, viewport.go:102-109; View slices m.lines), so without the branch the drawn frame keeps old-width wrapping and lines overflow a shrunk terminal — directly observable output corruption. (2) The View-time SetContent never persists: helpView (dashboard.go:1113) and tailScreen have value receivers, so their SetContent mutates a render-time copy discarded after the frame string is built, while the WindowSizeMsg branch runs inside Update whose modified model is retained. Delete the branches and the persisted tailView.lines stay wrapped at the old width, so scroll keys j/k/pgup/pgdn (dashboard.go:416-461) clamp YOffset against stale maxYOffset = len(lines)-Height (viewport.go:113-115): after a window shrink the bottom of the help text becomes unreachable, and AtBottom misreads change followActivity's unpause-on-scroll (dashboard.go:433, 458) and refreshLog's stick-to-bottom (dashboard.go:1205). Additionally, while a dialog is open renderView (dashboard.go:701-702) draws dialogView instead of any mode screen, another window where only the branch refreshes state. The finding's ModeDiff/ModeLog analysis is correct, but the ModeHelp and ModeTail branches are not redundant.

- **NUL-delimited git -z output is split and filtered three separate ways** (`internal/looper/git.go`)
  Reason: The deduplication idea is sound and the loop rewrites at git.go:186 and 202 are behavior-preserving, but the proposed `return splitNUL(b), nil` in Changes (git.go:261-273) is not behavior-preserving as specified. Changes currently returns `out := []string{}` — non-nil even when the diff is empty. Its only caller, engine.go:595, assigns the result to `r.Summary.Files`. Summary.Files serializes as `json:"files"` with no omitempty (types.go:336), so a nil slice writes `"files": null` instead of `"files": []` into summary.json and receipt.json, and `Summary.Validate` (types.go:343) explicitly rejects `s.Files == nil`. A helper that "drops empty entries" — the natural `var out []string` + append over `bytes.Split` — returns nil for an empty diff (commit tree equal to base tree). On recovery, engine.go:326-330 unmarshals receipt.json and calls `receipt.Summary.Validate`, which then fails and aborts engine recovery with "invalid summary for task %s". The codebase shows the non-nil convention is deliberate: hold() constructs `Files: []string{}` explicitly (engine.go:767). The helper contract must pin down non-nilness, and the finding's 5-LOC savings is overstated once the ~7-line helper is counted.

## Deferred findings (19, unverified)

These fell below the verification cap, ordered by estimated value. None has been
adversarially checked. Treat them as leads, not facts.

- `internal/ui/dashboard.go:757` | duplication | ~5 lines — elapsed and ageLabel re-implement the same coarse duration formatter
- `internal/looper/config.go:355` | duplication | ~4 lines — Auto-jobs policy duplicated inline in Parse instead of calling autoJobs()
- `internal/looper/imports.go:518` | over-abstraction | ~4 lines — sortStrings wraps sort.Strings for a single call site
- `internal/looper/dashboard.go:341` | needless-defense | ~4 lines — readDashboardLog stats the file before opening it
- `internal/looper/agent.go:129` | duplication | ~4 lines — Claude argv constructed twice; planning branch duplicates the base flag list
- `internal/looper/requirements.go:161` | needless-defense | ~4 lines — analyzeImport normalizes task status through a conditional that always assigns the same value
- `internal/looper/daemon.go:174` | redundant-state | ~3 lines — Detached-mode markers are written but never read
- `internal/looper/settings.go:447` | duplication | ~3 lines — patchProfile hand-copies all eleven Profile fields; use a struct copy
- `internal/looper/imports.go:253` | dead-code | ~3 lines — ackPlanImport status override branch can never change the value
- `internal/looper/integration.go:19` | duplication | ~3 lines — prepare repeats e.Repo.Remove(work) on all four error paths
- `internal/looper/process_unix.go:71` | dead-code | ~3 lines — Dead nil check on os.NewFile with constant fd 3
- `internal/ui/dashboard.go:88` | unused-field | ~3 lines — drainAsked field is written twice and never read
- `internal/looper/engine.go:29` | unused-field | ~2 lines — Engine.Agent field is set but never read
- `internal/looper/view.go:52` | needless-defense | ~2 lines — Nil guard on e.State cannot trigger in snapshot
- `internal/looper/process_unix.go:102` | stdlib-replacement | ~2 lines — Manual map copy loop can use maps.Copy
- `internal/looper/imports.go:473` | needless-defense | ~1 lines — applyProposal calls Validate a second time and discards the result
- `internal/looper/dashboard.go:23` | dead-code | ~1 lines — Dead store to c.Command and c.Follow in runTUI
- `internal/ui/dashboard.go:51` | unused-field | ~1 lines — Controls.Session is never set by any consumer and never read
- `internal/ui/dashboard.go:715` | dead-code | ~1 lines — Dead assignment parts = nil in header()

## Gap areas (from the completeness critic)

Areas the seven finders did not cover. The critic verified the concrete items by
reading code, but they did not pass the adversarial gate.

### internal/looper timing/statistics/usage/pricing subsystem (new untracked files)

About 1,300 lines of new code received zero findings: /home/agent/looper/internal/looper/timing.go (328 lines), statistics.go (257), usage.go (268), pricing.go (205), prices/, plus /home/agent/looper/internal/ui/statistics.go (240). No finder scope listed these files. Concrete missed items: statistics.go:104-108 re-inlines the process-liveness predicate (runIdentityCommand plus identity compare) that timing.go:173-178 already packages as the alive callback of Store.timing; the coarse duration formatter is re-implemented again in timing.go:320-324 (printTiming closure) and internal/ui/statistics.go:65-80 (statDuration), on top of the two copies the review deferred in internal/ui/dashboard.go; usage.go:138-143 calls tokenField twice on the same "cached_input_tokens" key and repeats the optional-key validation idiom already written at usage.go:77-83; pricing.go:156-159 has an unreachable branch because any non-exact selected model already satisfied m.Match.matches(model) at pricing.go:151.

Why it matters: This is the largest block of new code in the working tree (the LOOP-CORRECTNESS work) and it repeats the exact duplication categories the review confirmed elsewhere: copied predicates, repeated formatters, dead branches. The seven finder scopes were assigned by area name and none matches these four new files, so the whole subsystem went unexamined.

### File-based request/acknowledge protocol (spans settings, imports, dashboard_data)

The claim-process-acknowledge loop is written three times: settings.go:661-677 (processSettingsRequests), dashboard_data.go:188-205 (processRequeueRequests), and imports.go:138-149 (claimPlanImports). Each repeats claimNextRequest, nil check, apply, writeJSON into ackDir(e.Dir), and os.Remove of the processing file. The ack prologue is also triplicated: settings.go:507-522, dashboard_data.go:209-224, and imports.go:153-165 (acceptPlanImport) each re-implement replay-of-recorded-ack-by-ID and stale-session rejection. dashboard_data.go (262 lines, includes applyRequeueRequest and requeueTask) produced zero findings from any finder.

Why it matters: The duplication spans three different finders' scopes (settings, imports, dashboard), so each finder saw only one copy and none reported it. One generic processRequests(type, apply) helper would remove roughly 60 lines and make the fourth request type cheap to add.

### Test scaffolding (6,760 lines of _test.go, no finder scope)

No finder scope included test files. The engine bootstrap block (NewEngine(c, io.Discard), initialize with lock.Close on failure, t.Cleanup closing Store and lock) is copy-pasted 11 times across test files: git_test.go:22-30, diagnosis_test.go:29-36, review_test.go:32-41, resume_test.go:31-40, imports_test.go:81-90 and 303-312, inputwait_test.go:37-46, assessment_test.go:131-140, daemon_test.go:30-38, dashboard_test.go:78-86, publication_snapshot_test.go:23-31. Each file also wraps it in a private variant helper (idleEngine, reviewEngine, resumeEngine, importEngine, protocolEngine, requeueEngine, snapshotEngine, preparedRepo) that repeats the fakeAgent, repoConfig, CodexBin/ClaudeBin, and writeJSON-backlog wiring.

Why it matters: Tests are about 35 percent of the Go code, and the task names test-file bloat as an unexamined category. A single shared newTestEngine helper would remove roughly 100 duplicated lines and keep future request types from adding another copy.

### Install packaging (install.sh, uninstall.sh, Formula/)

install.sh:1-136 and uninstall.sh:1-131 share about 105 near-verbatim lines: the usage heredoc skeleton, the 42-line option-parsing loop (install.sh:36-77 vs uninstall.sh:36-77), the PREFIX/BIN_DIR/SKILLS_DIR resolution (both lines 79-105), and the run() dry-run helper. Only variable names (INSTALL_BIN vs REMOVE_BIN) and a few help strings differ. The Go build invocation with the version ldflags is copy-pasted three times: Makefile:8, install.sh:148-149, and Formula/looper.rb:12-14.

Why it matters: The repo-level finder covered the Makefile and scripts/ but not the top-level install scripts or the Formula directory, so no scope owned them. A sourced common.sh (or one script with install and uninstall verbs) plus a single shared build command would remove about 100 lines and keep the package path in one place when it changes.

## Applying

1. Re-run `go test ./...` to confirm the baseline still holds.
2. Apply by text anchor; line numbers have drifted since verification.
3. Land changes in small commits, one theme per commit, with the suite green
   after each.
4. Start with the dead-code deletions (findings 1-3): they cannot change
   behavior.
