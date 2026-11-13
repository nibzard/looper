package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/nibzard/looper/internal/contractschema"
)

// The AX-023 acceptance fixtures. Every fixture drives the public CLI:
// `looper preflight` resolves the execution configuration a start would use
// while starting no controller and no agent, changing no saved state, and
// keeping credentials, environment values, and raw agent arguments out.

// preflightSchema reads the published preflight schema, anchored at this
// file's directory because fixtures chdir into temporary projects.
func preflightSchema(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("the test file location is unavailable")
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "..", "..", "docs", "contracts", "schemas", "v2", "preflight.schema.json"))
	if err != nil {
		t.Skipf("preflight schema is absent: %v", err)
	}
	return b
}

// preflightScratch builds a scratch git project and parses a command in it,
// with agent binaries pinned to controlled paths so PATH differences between
// machines cannot change the findings.
func preflightScratch(t *testing.T, args ...string) Config {
	t.Helper()
	clearSettingEnv(t)
	bins := t.TempDir()
	for _, name := range []string{"codex", "claude", "zcode"} {
		if err := os.WriteFile(filepath.Join(bins, name), []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODEX_BIN", filepath.Join(bins, "codex"))
	t.Setenv("CLAUDE_BIN", filepath.Join(bins, "claude"))
	t.Setenv("ZCODE_BIN", filepath.Join(bins, "zcode"))
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "base"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_BASE_DIR", filepath.Join(root, "base"))
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	if _, err = gitText(root, "init", "-q"); err != nil {
		t.Fatal(err)
	}
	c, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// runPreflightCLI runs preflight through Main and returns the exit code, the
// document, and the raw output.
func runPreflightCLI(t *testing.T, jsonForm bool, args ...string) (int, *preflightDocument, string) {
	t.Helper()
	if jsonForm {
		args = append(args, "--contract", "v2", "--json")
	}
	var out, stderr bytes.Buffer
	code := Main(append([]string{"preflight"}, args...), &out, &stderr)
	if !jsonForm {
		return code, nil, out.String()
	}
	if stderr.Len() > 0 {
		t.Fatalf("the preflight JSON mixes human text into stdout: stderr=%q", stderr.String())
	}
	var doc preflightDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not one preflight document: %v\n%s", err, out.String())
	}
	dec := json.NewDecoder(bytes.NewReader(out.Bytes()))
	var first any
	if err := dec.Decode(&first); err != nil || dec.More() {
		t.Fatalf("stdout carries more than one JSON value: %v", err)
	}
	if err := contractschema.ValidateJSON(preflightSchema(t), out.Bytes()); err != nil {
		t.Fatalf("the emitted preflight violates its schema: %v\n%s", err, out.String())
	}
	if out.Len() > preflightBytesBudget {
		t.Fatalf("preflight document is %d bytes, budget %d", out.Len(), preflightBytesBudget)
	}
	return code, &doc, out.String()
}

// snapshotTree records every file under root with its size, so a no-mutation
// proof notices a created, removed, or rewritten file.
func snapshotTree(t *testing.T, root string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			out[strings.TrimPrefix(path, root)] = info.Size()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestPreflightChangesNothing proves the preview is read-only: no runtime
// directory, no saved state, no controller record, no lock file, no profile,
// and no change inside .git. Both renderings must leave the trees identical.
func TestPreflightChangesNothing(t *testing.T) {
	c := preflightScratch(t, "preflight")
	_, dir := projectPaths(c)
	baseBefore := snapshotTree(t, c.BaseDir)
	gitBefore := snapshotTree(t, filepath.Join(c.Root, ".git"))
	for _, jsonForm := range []bool{false, true} {
		code, _, _ := runPreflightCLI(t, jsonForm)
		if code != 0 {
			t.Fatalf("preflight exit %d on a healthy project", code)
		}
	}
	if after := snapshotTree(t, c.BaseDir); fmt.Sprint(after) != fmt.Sprint(baseBefore) {
		t.Fatalf("preflight changed the runtime base directory:\nbefore %v\nafter  %v", baseBefore, after)
	}
	if after := snapshotTree(t, filepath.Join(c.Root, ".git")); fmt.Sprint(after) != fmt.Sprint(gitBefore) {
		t.Fatalf("preflight changed .git:\nbefore %v\nafter  %v", gitBefore, after)
	}
	for _, created := range []string{
		dir,
		filepath.Join(dir, "state.db"),
		filepath.Join(dir, "controller.json"),
		c.ProfilePath,
	} {
		if _, err := os.Stat(created); err == nil {
			t.Errorf("preflight created %s", created)
		}
	}
}

// TestPreflightProvenanceLayers pins the sources: a saved profile supplies
// the lowest layer, a present environment variable wins over it, an explicit
// flag wins over both, and an empty review agent reports as derived from the
// coding agent instead of pretending a choice.
func TestPreflightProvenanceLayers(t *testing.T) {
	c := preflightScratch(t, "preflight")
	if err := SaveProfile(c.ProfilePath, &Profile{Version: ProfileVersion, Agent: "claude", ClaudeModel: "profile-model", Jobs: 3}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOOPER_ITER_SCHEDULE", "codex")
	t.Setenv("CLAUDE_MODEL", "env-model")
	code, doc, _ := runPreflightCLI(t, true, "--agent", "claude", "--review-agent", "codex")
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if !doc.Project.Profile.Present {
		t.Error("the saved profile must report present")
	}
	if doc.Agents.Coding.Name != "claude" || doc.Agents.Coding.Source != "flag" {
		t.Errorf("coding agent provenance wrong: %+v", doc.Agents.Coding)
	}
	if doc.Agents.Review.Name != "codex" || doc.Agents.Review.Source != "flag" {
		t.Errorf("review agent provenance wrong: %+v", doc.Agents.Review)
	}
	if doc.Settings.ClaudeModel.Value != "env-model" || doc.Settings.ClaudeModel.Source != "environment" {
		t.Errorf("the environment must win over the profile: %+v", doc.Settings.ClaudeModel)
	}
	if doc.Settings.Jobs.Value != 3 || doc.Settings.Jobs.Source != "profile" {
		t.Errorf("jobs must come from the profile: %+v", doc.Settings.Jobs)
	}
	present := map[string]bool{}
	for _, e := range doc.Environment {
		present[e.Name] = e.Present
	}
	if !present["LOOPER_ITER_SCHEDULE"] || !present["CLAUDE_MODEL"] || present["LOOPER_JOBS"] {
		t.Errorf("environment presence wrong: %v", present)
	}
	// An empty review agent reports as derived: the same value follows the
	// coding agent without a choice for the review role.
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Agents.Review.Name != doc.Agents.Coding.Name || doc.Agents.Review.Source != "derived" || !doc.Agents.Review.FollowsCoding {
		t.Errorf("an empty review agent must report derived from the coding agent: %+v", doc.Agents.Review)
	}
}

// TestPreflightMissingTools pins the behavior under missing tools: the agent
// binary reports missing with its configured request and source, the finding
// is an error, and the exit code reports the block like doctor. Nothing is
// started to find this out.
func TestPreflightMissingTools(t *testing.T) {
	preflightScratch(t, "preflight")
	t.Setenv("CLAUDE_BIN", "/nonexistent/claude-missing")
	code, doc, out := runPreflightCLI(t, true)
	if code != 1 {
		t.Fatalf("a missing coding agent binary must exit 1, got %d", code)
	}
	if doc.Agents.Coding.Binary.State != "missing" || doc.Agents.Coding.Binary.Requested != "/nonexistent/claude-missing" || doc.Agents.Coding.Binary.Source != "environment" {
		t.Errorf("the missing binary must report its request and source: %+v", doc.Agents.Coding.Binary)
	}
	var missing []preflightFinding
	for _, f := range doc.Findings {
		if f.Code == "executable_missing" && f.Severity == "error" {
			missing = append(missing, f)
		}
	}
	if len(missing) == 0 {
		t.Fatalf("no executable_missing error finding in %s", out)
	}
	if !strings.Contains(out, "/nonexistent/claude-missing") {
		t.Errorf("the finding must name the configured request: %s", out)
	}
	// Plain output renders the same finding.
	code, _, plain := runPreflightCLI(t, false)
	if code != 1 || !strings.Contains(plain, "executable_missing") {
		t.Fatalf("plain preflight must render the finding and exit 1: exit %d\n%s", code, plain)
	}
}

// TestPreflightProviderDefaults pins unknown provider defaults: an unset
// model reports provider_default and lands in unchecked, an explicitly set
// model reports configured, and a model for an agent kind that is not
// configured reports not_applicable.
func TestPreflightProviderDefaults(t *testing.T) {
	preflightScratch(t, "preflight")
	code, doc, _ := runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Settings.ClaudeModel.Resolution != "provider_default" {
		t.Errorf("an unset claude model must report provider_default: %+v", doc.Settings.ClaudeModel)
	}
	if doc.Settings.CodexModel.Resolution != "not_applicable" {
		t.Errorf("with no codex agent the codex model must report not_applicable: %+v", doc.Settings.CodexModel)
	}
	unchecked := map[string]bool{}
	for _, u := range doc.Unchecked {
		unchecked[u.Code] = true
	}
	if !unchecked["agent_authentication"] {
		t.Errorf("agent_authentication must stay explicitly unchecked: %+v", doc.Unchecked)
	}
	if !unchecked["provider_default_models"] {
		t.Errorf("unset models must stay explicitly unchecked: %+v", doc.Unchecked)
	}
	if !unchecked["verify_command"] {
		t.Errorf("a planner-chosen verify command must stay explicitly unchecked: %+v", doc.Unchecked)
	}
	t.Setenv("CLAUDE_MODEL", "claude-value")
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Settings.ClaudeModel.Value != "claude-value" || doc.Settings.ClaudeModel.Resolution != "configured" || doc.Settings.ClaudeModel.Source != "environment" {
		t.Errorf("an explicitly set model must report configured with its source: %+v", doc.Settings.ClaudeModel)
	}
}

// TestPreflightOmitsSecrets pins the omission rules: credential shapes inside
// a verify command are redacted, environment values never ride along, and no
// raw agent argument appears anywhere in the document.
func TestPreflightOmitsSecrets(t *testing.T) {
	preflightScratch(t, "preflight")
	secret := "ghp_abcd1234efgh5678ijkl"
	t.Setenv("LOOPER_VERIFY_COMMAND", fmt.Sprintf("curl -H %q https://example.test/api", "Authorization: Bearer "+secret))
	_, doc, raw := runPreflightCLI(t, true)
	if strings.Contains(raw, secret) {
		t.Fatalf("the credential shape entered the preflight document: %s", raw)
	}
	if !strings.Contains(doc.Commands.Verify.Value, "[redacted]") {
		t.Errorf("the verify command must be scrubbed, got %q", doc.Commands.Verify.Value)
	}
	// A URL-encoded separator carries the same credential the space does;
	// the raw token must not ride along behind it.
	t.Setenv("LOOPER_SETUP_COMMAND", fmt.Sprintf("curl https://example.test/setup?token=Bearer%%20%s", secret))
	_, _, raw = runPreflightCLI(t, true)
	if strings.Contains(raw, secret) {
		t.Fatalf("the encoded credential shape entered the preflight document: %s", raw)
	}
	// Argument carriers outside assignments — a curl user flag and a
	// hyphenated key header — are credentials the document must omit too.
	t.Setenv("LOOPER_SETUP_COMMAND", "curl -u deploy:hunter2-secret https://example.test/x")
	t.Setenv("LOOPER_VERIFY_COMMAND", `curl -H "X-Api-Key: abcdef123456789" https://example.test/y`)
	_, _, raw = runPreflightCLI(t, true)
	for _, leaked := range []string{"hunter2-secret", "abcdef123456789"} {
		if strings.Contains(raw, leaked) {
			t.Fatalf("the credential carrier %q entered the preflight document: %s", leaked, raw)
		}
	}
	for _, banned := range []string{"dangerously", "--sandbox", "bypass"} {
		if strings.Contains(raw, banned) {
			t.Errorf("raw agent argument %q entered the document: %s", banned, raw)
		}
	}
	code, _, plain := runPreflightCLI(t, false)
	if code != 0 || strings.Contains(plain, secret) {
		t.Fatalf("plain output leaked the secret (exit %d):\n%s", code, plain)
	}
	if !strings.Contains(plain, "[redacted]") {
		t.Errorf("plain output must render the scrubbed command:\n%s", plain)
	}
	if !strings.Contains(plain, "values are never shown") {
		t.Errorf("plain output must state that environment values are never shown:\n%s", plain)
	}
}

// TestPreflightSavedRunCommands pins verification provenance against a saved
// run: the saved plan supplies the planner's command, and a recovered
// command applies only while the configuration still matches the value it
// recovered from.
func TestPreflightSavedRunCommands(t *testing.T) {
	c := preflightScratch(t, "preflight")
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &State{Version: 2, Root: c.Root, Todo: c.Todo, Head: "h", Branch: "b", Tip: "t", Published: "p",
		Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}, Attempts: []*Attempt{},
		Plan: &Plan{Setup: "make setup", Verify: "make check", Tasks: []Scope{{TaskID: "T1", Paths: []string{"."}}}}}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	code, doc, _ := runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Commands.Verify.Value != "make check" || doc.Commands.Verify.Source != "plan" {
		t.Errorf("the saved plan must supply the verify command: %+v", doc.Commands.Verify)
	}
	if doc.Commands.Setup.Value != "make setup" || doc.Commands.Setup.Source != "plan" {
		t.Errorf("the saved plan must supply the setup command: %+v", doc.Commands.Setup)
	}
	// A recovered command replaces the value it recovered from; a different
	// current configuration no longer matches, so the plan applies again.
	s.RecoveredCommands = &RecoveryCommands{BeforeSetup: "", Setup: "make recovered-setup", BeforeVerify: "", Verify: "make recovered-verify"}
	store, err = OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Commands.Verify.Value != "make recovered-verify" || doc.Commands.Verify.Source != "recovered" {
		t.Errorf("the recovered verify command must apply: %+v", doc.Commands.Verify)
	}
	// An explicit environment value outranks both saved layers.
	t.Setenv("LOOPER_VERIFY_COMMAND", "make explicit")
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Commands.Verify.Value != "make explicit" || doc.Commands.Verify.Source != "environment" {
		t.Errorf("an explicit verify command must win: %+v", doc.Commands.Verify)
	}
	// Engine.capture replaces a configured value that still equals the value
	// the recovery recovered from, even when that value is not empty; the
	// preview reports the recovered value. A recovery that cleared the value
	// hands the command back to the plan.
	s.RecoveredCommands = &RecoveryCommands{BeforeVerify: "make explicit", Verify: "make recovered-verify"}
	store, err = OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Commands.Verify.Value != "make recovered-verify" || doc.Commands.Verify.Source != "recovered" {
		t.Errorf("the recovered command must replace the matching configured value: %+v", doc.Commands.Verify)
	}
	s.RecoveredCommands = &RecoveryCommands{BeforeVerify: "make explicit", Verify: ""}
	store, err = OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Commands.Verify.Value != "make check" || doc.Commands.Verify.Source != "plan" {
		t.Errorf("a cleared recovery must hand the verify command back to the plan: %+v", doc.Commands.Verify)
	}
	// A saved run without a plan rebuilds it at the next planning pass; the
	// commands report the planner until then.
	s.Plan, s.RecoveredCommands = nil, nil
	store, err = OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	os.Unsetenv("LOOPER_VERIFY_COMMAND")
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("preflight exit %d: %+v", code, doc.Findings)
	}
	if doc.Commands.Verify.Source != "planner" || doc.Commands.Setup.Source != "planner" {
		t.Errorf("without a saved plan both commands must report planner: %+v", doc.Commands)
	}
}

// TestPreflightCommandExecutables pins how command executables resolve: a
// found command reports its path, a missing one a warning, and compound
// commands stay unchecked instead of guessing.
func TestPreflightCommandExecutables(t *testing.T) {
	preflightScratch(t, "preflight")
	t.Setenv("LOOPER_VERIFY_COMMAND", "looper-no-such-verify-tool test")
	t.Setenv("LOOPER_SETUP_COMMAND", "cd sub && make setup")
	code, doc, _ := runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("a missing verify executable is a warning, not a block: %+v", doc.Findings)
	}
	if e := doc.Commands.Verify.Executable; e == nil || e.State != "missing" || e.Requested != "looper-no-such-verify-tool" {
		t.Errorf("the verify command's executable must resolve through PATH: %+v", doc.Commands.Verify.Executable)
	}
	warned := false
	for _, f := range doc.Findings {
		if f.Code == "command_executable_missing" && f.Severity == "warning" {
			warned = true
		}
		if f.Severity == "error" {
			t.Errorf("a command executable problem must not block: %+v", f)
		}
	}
	if !warned {
		t.Errorf("the missing command executable must warn: %+v", doc.Findings)
	}
	if e := doc.Commands.Setup.Executable; e == nil || e.State != "builtin" {
		t.Errorf("a command led by a shell builtin must report builtin: %+v", doc.Commands.Setup.Executable)
	}
	// The remaining command-word states, probed directly. A compound whose
	// first word is plain resolves that word; only a first word PATH cannot
	// decide stays unchecked.
	plainClamp := preflightClampOf(preflightFits[0])
	for command, want := range map[string]string{
		"FOO=1 make test": "unchecked",
		"cd sub && make":  "builtin",
	} {
		if e := probeCommand(command, plainClamp); e == nil || e.State != want {
			t.Errorf("probeCommand(%q) must report %s, got %+v", command, want, e)
		}
	}
	if e := probeCommand("sh -c make", plainClamp); e == nil || e.State != "found" || e.Resolved == "" {
		t.Errorf("an installed command must report its path: %+v", e)
	}
}

// TestPreflightBoundsBinaryFields pins the budget rule on the fields the
// environment supplies (T102): a requested binary path far over the string
// bound, its resolved path, and a command's first word enter the document
// clamped, the emitted document stays at or under the budget its limits
// state, and escape-heavy values shrink by emitted bytes, not raw bytes.
func TestPreflightBoundsBinaryFields(t *testing.T) {
	preflightScratch(t, "preflight")
	// The finding's reproduction: a 9000-character request entered the
	// document verbatim while limits still reported the 8192-byte budget.
	t.Setenv("CLAUDE_BIN", strings.Repeat("a", 9000))
	code, doc, raw := runPreflightCLI(t, true)
	if code != 1 {
		t.Fatalf("the oversized missing binary keeps its error finding, got exit %d", code)
	}
	if e := doc.Agents.Coding.Binary; e.State != "missing" || jsonEscapedLen(e.Requested) > doc.Limits.StringMax {
		t.Errorf("the requested binary must report clamped: %+v (limits %+v)", e, doc.Limits)
	}
	if doc.Limits.BytesBudget < len(raw) {
		t.Errorf("limits report a %d byte budget for a %d byte document", doc.Limits.BytesBudget, len(raw))
	}
	// Escape-heavy content: every ampersand emits six bytes, so a raw-byte
	// clamp would let 256 recorded bytes become 1536 emitted ones.
	t.Setenv("CLAUDE_BIN", strings.Repeat("amp&<>\x01", 500))
	code, doc, raw = runPreflightCLI(t, true)
	if code != 1 {
		t.Fatalf("the escape-heavy missing binary keeps its error finding, got exit %d", code)
	}
	if e := doc.Agents.Coding.Binary; jsonEscapedLen(e.Requested) > doc.Limits.StringMax {
		t.Errorf("the requested value must clamp by emitted bytes: %d emitted, bound %d",
			jsonEscapedLen(e.Requested), doc.Limits.StringMax)
	}
	for _, f := range doc.Findings {
		if jsonEscapedLen(f.Message) > doc.Limits.StringMax {
			t.Errorf("finding %s message exceeds the stated bound: %d emitted", f.Code, jsonEscapedLen(f.Message))
		}
	}
	// A resolved path is content the filesystem supplied; it clamps too.
	long := t.TempDir()
	segment := strings.Repeat("d", 150)
	for i := 0; i < 3; i++ {
		long = filepath.Join(long, segment)
	}
	if err := os.MkdirAll(long, 0755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(long, "claude")
	if err := os.WriteFile(claude, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_BIN", claude)
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("the long-path binary resolves, got exit %d: %+v", code, doc.Findings)
	}
	if e := doc.Agents.Coding.Binary; e.State != "found" || !strings.HasSuffix(e.Resolved, "…") || jsonEscapedLen(e.Resolved) > doc.Limits.StringMax {
		t.Errorf("the resolved path must report clamped: state %s, %d emitted bytes, bound %d",
			e.State, jsonEscapedLen(e.Resolved), doc.Limits.StringMax)
	}
	// A command word over the bound clamps the same way, on both sides of
	// the PATH decision.
	t.Setenv("LOOPER_VERIFY_COMMAND", strings.Repeat("x", 9000)+" argument")
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("a missing command executable is a warning, got exit %d", code)
	}
	if e := doc.Commands.Verify.Executable; e == nil || jsonEscapedLen(e.Requested) > doc.Limits.StringMax {
		t.Errorf("the command word must report clamped: %+v (limits %+v)", e, doc.Limits)
	}
}

// preflightEscapeHeavy is one string family that maximizes JSON escaping:
// HTML-significant characters, a control character, and a line separator all
// expand after the clamp, to six emitted bytes per recorded rune at worst.
func preflightEscapeHeavy(n int) string { return strings.Repeat("amp&<>\x01", n) }

// TestPreflightBoundsEscapeExpandedContent pins the whole document under
// escape-heavy configuration: one schema-valid document at or under the
// budget, every content-derived field bounded by the emitted bytes its
// limits state, and the plain rendering readable and bounded too.
func TestPreflightBoundsEscapeExpandedContent(t *testing.T) {
	args := []string{"preflight"}
	for i := 0; i < 9; i++ {
		args = append(args, "--plan", filepath.Join(t.TempDir(), preflightEscapeHeavy(60)))
	}
	preflightScratch(t, args...)
	t.Setenv("CLAUDE_BIN", preflightEscapeHeavy(400))
	t.Setenv("LOOPER_VERIFY_COMMAND", preflightEscapeHeavy(400)+" make check")
	t.Setenv("LOOPER_SETUP_COMMAND", "cd "+preflightEscapeHeavy(400)+" && make setup")
	t.Setenv("CLAUDE_MODEL", preflightEscapeHeavy(400))
	code, doc, raw := runPreflightCLI(t, true)
	if code != 1 {
		t.Fatalf("the missing escape-heavy binary keeps its error finding, got exit %d", code)
	}
	if doc.Limits.BytesBudget < len(raw) {
		t.Errorf("limits report a %d byte budget for a %d byte document", doc.Limits.BytesBudget, len(raw))
	}
	for _, bounded := range []struct {
		label string
		value string
	}{
		{"coding binary requested", doc.Agents.Coding.Binary.Requested},
		{"claude model", doc.Settings.ClaudeModel.Value},
		{"verify command", doc.Commands.Verify.Value},
		{"setup executable", func() string {
			if e := doc.Commands.Setup.Executable; e != nil {
				return e.Requested
			}
			return ""
		}()},
	} {
		if jsonEscapedLen(bounded.value) > doc.Limits.StringMax {
			t.Errorf("%s exceeds the stated bound: %d emitted, bound %d",
				bounded.label, jsonEscapedLen(bounded.value), doc.Limits.StringMax)
		}
	}
	for _, f := range doc.Findings {
		if jsonEscapedLen(f.Message) > doc.Limits.StringMax {
			t.Errorf("finding %s message exceeds the stated bound: %d emitted", f.Code, jsonEscapedLen(f.Message))
		}
	}
	for _, plan := range doc.Execution.Plans {
		if jsonEscapedLen(plan) > doc.Limits.StringMax {
			t.Errorf("a plan path exceeds the stated bound: %d emitted", jsonEscapedLen(plan))
		}
	}
	code, _, plain := runPreflightCLI(t, false)
	if code != 1 || !strings.HasPrefix(plain, "Preflight (read-only") {
		t.Fatalf("plain preflight must render the same findings (exit %d):\n%s", code, plain)
	}
	if len(plain) > 4*preflightBytesBudget {
		t.Fatalf("plain rendering of the bounded document is not bounded: %d bytes", len(plain))
	}
}

// TestPreflightTerminalFitBoundsAnyContent pins the structural guarantee
// behind the budget: the ladder's terminal fit bounds every content-derived
// string to one rune at worst, so the document it renders is structure alone.
// Every channel that can carry content — binaries, models, effort, commands,
// startup plans, the saved plan, recovered commands — carries escape-heavy
// text far over every bound, and the terminal document and the ladder's
// chosen document must both stay under the budget and validate against the
// published schema, whose enums no fit may truncate.
func TestPreflightTerminalFitBoundsAnyContent(t *testing.T) {
	args := []string{"preflight"}
	for i := 0; i < 9; i++ {
		args = append(args, "--plan", filepath.Join(t.TempDir(), preflightEscapeHeavy(80)))
	}
	c := preflightScratch(t, args...)
	t.Setenv("CLAUDE_BIN", preflightEscapeHeavy(400))
	t.Setenv("CODEX_BIN", preflightEscapeHeavy(400))
	t.Setenv("ZCODE_BIN", preflightEscapeHeavy(400))
	t.Setenv("CLAUDE_MODEL", preflightEscapeHeavy(400))
	t.Setenv("CODEX_MODEL", preflightEscapeHeavy(400))
	t.Setenv("CODEX_REASONING_EFFORT", preflightEscapeHeavy(400))
	t.Setenv("LOOPER_VERIFY_COMMAND", preflightEscapeHeavy(400)+" make check")
	t.Setenv("LOOPER_SETUP_COMMAND", "cd "+preflightEscapeHeavy(400)+" && make setup")
	_, dir := projectPaths(c)
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &State{Version: 2, Root: c.Root, Todo: c.Todo, Head: "h", Branch: "b", Tip: "t", Published: "p",
		Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}, Attempts: []*Attempt{},
		Plan: &Plan{Setup: preflightEscapeHeavy(300), Verify: preflightEscapeHeavy(300), Tasks: []Scope{{TaskID: "T1", Paths: []string{"."}}}},
		RecoveredCommands: &RecoveryCommands{BeforeSetup: "", Setup: preflightEscapeHeavy(300),
			BeforeVerify: "", Verify: preflightEscapeHeavy(300)}}
	if err = store.Save(s); err != nil {
		t.Fatal(err)
	}
	store.Close()
	saved := readSavedRun(dir)
	if saved.unreadable != nil || saved.state == nil {
		t.Fatalf("the saved run must read: %+v", saved.unreadable)
	}
	// The earlier fits may legitimately overflow — refitting is the ladder's
	// job. The guarantees are that the terminal fit bounds any content and
	// that the ladder's chosen document fits whatever the earlier fits
	// measured.
	terminal := renderPreflight(c, dir, saved, preflightTerminalFit)
	remarshaled, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	if err = contractschema.ValidateJSON(preflightSchema(t), remarshaled); err != nil {
		t.Fatalf("the terminal document violates its schema (an enum literal must never truncate): %v\n%s", err, remarshaled)
	}
	if chosen := buildPreflight(c); preflightEncoded(chosen) > preflightBytesBudget {
		t.Fatalf("the ladder's chosen document exceeds the budget it reports: %d bytes", preflightEncoded(chosen))
	}
}

// TestPreflightUnreadableStatePath pins the provenance gap beside the budget:
// a state path that cannot be read for a reason other than absence keeps its
// reason as a state_unreadable warning, so the recovered-command and
// saved-plan layers are reported unknown instead of silently dropped. A
// symlink loop makes the stat fail without permission tricks, so the probe
// behaves the same for every user.
func TestPreflightUnreadableStatePath(t *testing.T) {
	// The absent path stays a clean no-saved-run answer.
	if s := readSavedRun(filepath.Join(t.TempDir(), "absent")); s.state != nil || s.unreadable != nil {
		t.Errorf("an absent store is no saved run, not an error: %+v", s)
	}
	c := preflightScratch(t, "preflight")
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("state.db", filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	if s := readSavedRun(dir); s.unreadable == nil {
		t.Fatal("a state path that cannot be stat'ed must keep its reason")
	}
	code, doc, _ := runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("an unreadable state path is a warning, got exit %d: %+v", code, doc.Findings)
	}
	warned := false
	for _, f := range doc.Findings {
		if f.Code == "state_unreadable" && f.Severity == "warning" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("the unreadable state path must warn state_unreadable: %+v", doc.Findings)
	}
	if doc.Commands.Verify.Source != "planner" {
		t.Errorf("with the saved run unreadable the verify command must report planner: %+v", doc.Commands.Verify)
	}
}

// TestPreflightContract pins the command contract: the version 2 document is
// the only JSON form, plain text renders under both contracts, and the
// findings-driven exit code carries to both renderings.
func TestPreflightContract(t *testing.T) {
	preflightScratch(t, "preflight")
	var out, stderr bytes.Buffer
	code := Main([]string{"preflight", "--json"}, &out, &stderr)
	if code != exitOperationFailed {
		t.Fatalf("legacy --json must refuse, got %d: %s%s", code, out.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "contract version 2") {
		t.Fatalf("the refusal must name the versioned document: %q", stderr.String())
	}
	out.Reset()
	stderr.Reset()
	code = Main([]string{"preflight", "--contract", "v2", "--json"}, &out, &stderr)
	if code != 0 {
		t.Fatalf("the version 2 document must serve, got %d: %s", code, out.String())
	}
	if err := contractschema.ValidateJSON(preflightSchema(t), out.Bytes()); err != nil {
		t.Fatalf("the version 2 preflight violates its schema: %v\n%s", err, out.String())
	}
	for _, args := range [][]string{{"preflight"}, {"preflight", "--contract", "legacy"}, {"preflight", "--contract", "v2"}} {
		out.Reset()
		stderr.Reset()
		code = Main(args, &out, &stderr)
		if code != 0 || !strings.HasPrefix(out.String(), "Preflight (read-only") {
			t.Fatalf("plain preflight %v must render the document (exit %d):\n%s", args, code, out.String())
		}
	}
}

// TestPreflightOwnershipAndUnreadableState pins the warning findings: a
// controller that owns the repository blocks a new start, and an unreadable
// saved run keeps the recovered commands unknown instead of guessed.
func TestPreflightOwnershipAndUnreadableState(t *testing.T) {
	c := preflightScratch(t, "preflight")
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockRepository(filepath.Join(c.Root, common))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	code, doc, _ := runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("ownership is a warning, not a block: %+v", doc.Findings)
	}
	var owned, blocked bool
	for _, f := range doc.Findings {
		if f.Code == "controller_owns_repository" && f.Severity == "warning" {
			owned, blocked = true, f.Action == "looper stop"
		}
	}
	if !owned || !blocked {
		t.Errorf("the ownership finding must name looper stop: %+v", doc.Findings)
	}
	lock.Close()
	_, dir := projectPaths(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	code, doc, _ = runPreflightCLI(t, true)
	if code != 0 {
		t.Fatalf("an unreadable saved run is a warning: %+v", doc.Findings)
	}
	for _, f := range doc.Findings {
		if f.Code == "state_unreadable" && f.Severity != "warning" {
			t.Errorf("the unreadable state finding must be a warning: %+v", f)
		}
	}
	if doc.Commands.Verify.Source != "planner" {
		t.Errorf("with the saved run unreadable the verify command must report planner: %+v", doc.Commands.Verify)
	}
}

// TestForegroundLaunchDescription pins the description emitted before
// foreground execution: plain output prints one concise line, version 2 JSON
// gets the same facts as one launch event, and the legacy JSON stream keeps
// its byte-compatible shape.
func TestForegroundLaunchDescription(t *testing.T) {
	preflightScratch(t, "run")
	var out bytes.Buffer
	c, err := Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	emitLaunchDescription(c, &out)
	if !strings.HasPrefix(out.String(), "Launch: agent ") || !strings.Contains(out.String(), "| once no | foreground") {
		t.Fatalf("the launch description is not concise and safe: %q", out.String())
	}
	for _, part := range []string{"review", "model", "jobs", "checks", "verify"} {
		if !strings.Contains(out.String(), part) {
			t.Errorf("the launch description omits %s: %q", part, out.String())
		}
	}
	out.Reset()
	c, err = Parse([]string{"run", "--contract", "v2", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	emitLaunchDescription(c, &out)
	var event Event
	if err := json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("the version 2 launch line is not one event: %v\n%s", err, out.String())
	}
	if event.Kind != "launch" || event.Message == "" || len(event.Data) == 0 {
		t.Fatalf("the launch event is incomplete: %+v", event)
	}
	var data map[string]any
	if err := json.Unmarshal(event.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["mode"] != "foreground" || data["coding_agent"] != "claude" {
		t.Fatalf("the launch event data is wrong: %v", data)
	}
	out.Reset()
	c, err = Parse([]string{"run", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	emitLaunchDescription(c, &out)
	if out.Len() != 0 {
		t.Fatalf("the legacy JSON stream must stay byte-compatible, got %q", out.String())
	}
}

// TestForegroundLaunchDescriptionCommands pins the launch line's command
// provenance on a resume: the line resolves verify and setup through the same
// saved-run precedence the engine applies (Engine.capture, then the saved
// plan), so it agrees with the preflight document on the same state instead
// of reporting the planner while a recovered or planned command will run.
func TestForegroundLaunchDescriptionCommands(t *testing.T) {
	c := preflightScratch(t, "run")
	_, dir := projectPaths(c)
	// Without a saved run the wording stays the concise default, and the
	// read the line performs creates no runtime directory.
	var out bytes.Buffer
	emitLaunchDescription(c, &out)
	if !strings.Contains(out.String(), "verify planner-chosen [planner]") || !strings.Contains(out.String(), "setup none [none]") {
		t.Fatalf("without a saved run the launch line must keep its planner wording: %q", out.String())
	}
	if _, err := os.Stat(dir); err == nil {
		t.Errorf("the launch description created the runtime directory %s", dir)
	}
	writeSaved := func(mutate func(*State)) {
		t.Helper()
		store, err := OpenStore(filepath.Join(dir, "state.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		s := &State{Version: 2, Root: c.Root, Todo: c.Todo, Head: "h", Branch: "b", Tip: "t", Published: "p",
			Backlog: Backlog{Version: 1, Sources: []string{}, Tasks: []Task{}}, Attempts: []*Attempt{},
			Plan: &Plan{Setup: "make plan-setup", Verify: "make plan-verify", Tasks: []Scope{{TaskID: "T1", Paths: []string{"."}}}}}
		mutate(s)
		if err = store.Save(s); err != nil {
			t.Fatal(err)
		}
	}
	// The saved plan alone supplies both commands.
	writeSaved(func(s *State) {})
	out.Reset()
	emitLaunchDescription(c, &out)
	if !strings.Contains(out.String(), "verify make plan-verify [plan]") || !strings.Contains(out.String(), "setup make plan-setup [plan]") {
		t.Fatalf("the saved plan must supply the launch line's commands: %q", out.String())
	}
	// A recovered command applies while the configuration still matches the
	// value it recovered from, and the line agrees with the preflight
	// document on the same state.
	writeSaved(func(s *State) {
		s.RecoveredCommands = &RecoveryCommands{BeforeSetup: "", Setup: "make recovered-setup", BeforeVerify: "", Verify: "make recovered-verify"}
	})
	_, doc, _ := runPreflightCLI(t, true)
	if doc.Commands.Verify.Value != "make recovered-verify" || doc.Commands.Verify.Source != "recovered" {
		t.Fatalf("preflight must report the recovered verify command: %+v", doc.Commands.Verify)
	}
	out.Reset()
	emitLaunchDescription(c, &out)
	for _, part := range []string{
		"verify " + doc.Commands.Verify.Value + " [" + doc.Commands.Verify.Source + "]",
		"setup " + doc.Commands.Setup.Value + " [" + doc.Commands.Setup.Source + "]",
	} {
		if !strings.Contains(out.String(), part) {
			t.Errorf("the launch line must agree with preflight (%s): %q", part, out.String())
		}
	}
	// The same facts reach the version 2 launch event.
	out.Reset()
	c, err := Parse([]string{"run", "--contract", "v2", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	emitLaunchDescription(c, &out)
	var event Event
	if err = json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("the launch line is not one event: %v\n%s", err, out.String())
	}
	var data map[string]any
	if err = json.Unmarshal(event.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data["verify"] != "make recovered-verify" || data["verify_source"] != "recovered" ||
		data["setup"] != "make recovered-setup" || data["setup_source"] != "recovered" {
		t.Fatalf("the launch event data misreports the commands: %v", data)
	}
	// An explicit environment value outranks both saved layers; a recovery
	// whose previous value no longer matches the configuration stops
	// applying, exactly as Engine.capture drops the override.
	t.Setenv("LOOPER_VERIFY_COMMAND", "make explicit")
	c, err = Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	emitLaunchDescription(c, &out)
	if !strings.Contains(out.String(), "verify make explicit [environment]") {
		t.Fatalf("an explicit verify command must win the launch line: %q", out.String())
	}
	// A recovery that replaced exactly that value reports the recovered one;
	// a recovery that cleared it hands the command back to the saved plan.
	writeSaved(func(s *State) {
		s.RecoveredCommands = &RecoveryCommands{BeforeVerify: "make explicit", Verify: "make recovered-verify"}
	})
	out.Reset()
	emitLaunchDescription(c, &out)
	if !strings.Contains(out.String(), "verify make recovered-verify [recovered]") {
		t.Fatalf("a matching configured value must report the recovered command: %q", out.String())
	}
	writeSaved(func(s *State) {
		s.RecoveredCommands = &RecoveryCommands{BeforeVerify: "make explicit", Verify: ""}
	})
	out.Reset()
	emitLaunchDescription(c, &out)
	if !strings.Contains(out.String(), "verify make plan-verify [plan]") {
		t.Fatalf("a cleared recovery must hand the verify command back to the plan: %q", out.String())
	}
}

// TestRunEmitsLaunchDescriptionBeforeExecution drives Main: the description
// is the first output a foreground run produces, before any controller can
// exist. The repository lock is held so the run cannot proceed past startup.
func TestRunEmitsLaunchDescriptionBeforeExecution(t *testing.T) {
	c := preflightScratch(t, "run")
	common, err := gitText(c.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := lockRepository(filepath.Join(c.Root, common))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out, stderr bytes.Buffer
	code := Main([]string{"run", "--contract", "v2", "--json"}, &out, &stderr)
	if code == 0 {
		t.Fatal("a run against a held lock must fail")
	}
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	var event Event
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("the first output line is not the launch event: %v\n%s", err, out.String())
	}
	if event.Kind != "launch" {
		t.Fatalf("the first output line is %q, not the launch event", event.Kind)
	}
	var failure errorDocument
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &failure); err != nil || failure.Kind != "looper.error" {
		t.Fatalf("the failed start must end with its error document: %v\n%s", err, out.String())
	}
	// The plain foreground run prints the description before any controller
	// output; the default-agent notice may precede it.
	out.Reset()
	stderr.Reset()
	Main([]string{"run"}, &out, &stderr)
	if !strings.Contains(out.String(), "Launch: agent ") {
		t.Fatalf("plain run output must carry the launch description:\n%s", out.String())
	}
}

// TestForegroundLaunchDescriptionBoundsConfiguredValues pins the launch
// line's own bound (the T102 class measured at preflight): the resolved
// binary path, the effective command text, and the model and effort values
// are configured content, and an oversized or escape-heavy value must not
// grow the plain line or the version 2 launch event without limit. Every
// value is clamped by its emitted bytes, so the whole line stays within a
// small constant multiple of the per-value bound.
func TestForegroundLaunchDescriptionBoundsConfiguredValues(t *testing.T) {
	preflightScratch(t, "run")
	esc := preflightEscapeHeavy
	t.Setenv("CLAUDE_BIN", strings.Repeat("x", 9000))
	t.Setenv("LOOPER_VERIFY_COMMAND", esc(400)+" make check")
	t.Setenv("LOOPER_SETUP_COMMAND", "cd "+esc(400)+" && make setup")
	t.Setenv("CLAUDE_MODEL", esc(400))
	t.Setenv("CODEX_REASONING_EFFORT", esc(400))
	c, err := Parse([]string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	line, data := launchDescription(c)
	if !strings.HasPrefix(line, "Launch: agent ") {
		t.Fatalf("the clamped launch line lost its shape: %q", line)
	}
	lineBound := 11 * launchStringMax
	if len(line) > lineBound {
		t.Fatalf("the launch line is %d bytes, over the %d byte bound the per-value clamp implies:\n%s",
			len(line), lineBound, line)
	}
	for key, value := range data {
		s, ok := value.(string)
		if !ok {
			continue
		}
		if jsonEscapedLen(s) > launchStringMax {
			t.Errorf("launch value %s exceeds the %d byte bound: %d emitted", key, launchStringMax, jsonEscapedLen(s))
		}
	}
	// A 9 000-character configured binary is the measured overflow case: it
	// must arrive clamped, not verbatim.
	if binary, _ := data["binary"].(string); len(binary) > launchStringMax {
		t.Errorf("the resolved binary path must clamp, got %d bytes", len(binary))
	}
	// The version 2 launch event carries the same bounded facts.
	var out bytes.Buffer
	c, err = Parse([]string{"run", "--contract", "v2", "--json"})
	if err != nil {
		t.Fatal(err)
	}
	emitLaunchDescription(c, &out)
	if out.Len() > 2*lineBound+512 {
		t.Fatalf("the launch event is %d bytes, over the bound the per-value clamp implies", out.Len())
	}
	var event Event
	if err = json.Unmarshal(out.Bytes(), &event); err != nil {
		t.Fatalf("the launch line is not one event: %v\n%s", err, out.String())
	}
	if event.Kind != "launch" || event.Message != line {
		t.Fatalf("the launch event must carry the same bounded line: %+v", event)
	}
}
