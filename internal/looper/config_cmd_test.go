package looper

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/nibzard/looper/internal/contractschema"
)

// plainConfig builds a configuration outside any Git repository, the way a
// machine-scope command runs.
func plainConfig(t *testing.T, args ...string) Config {
	t.Helper()
	clearSettingEnv(t)
	root := t.TempDir()
	t.Setenv("LOOPER_BASE_DIR", filepath.Join(root, "base"))
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	c, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func headlessConfig(t *testing.T, args ...string) Config {
	t.Helper()
	c, err := Parse(args)
	if err != nil {
		t.Fatal(err)
	}
	return v2Config(c, "config", true)
}

func decodeConfigDoc(t *testing.T, out *bytes.Buffer) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("document does not parse: %v\n%s", err, out.String())
	}
	return doc
}

// configSchema reads the schema through the test source location, because
// these tests change the working directory into temporary repositories.
func configSchema(t *testing.T) []byte {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source directory")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "docs", "contracts", "schemas", "v2", "config.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func assertConfigSchema(t *testing.T, out *bytes.Buffer) {
	t.Helper()
	if err := contractschema.ValidateJSON(configSchema(t), out.Bytes()); err != nil {
		t.Fatalf("document must match the config schema: %v\n%s", err, out.String())
	}
}

// configErrText renders the version 2 error document a refused command
// writes to standard output.
func configErrText(t *testing.T, out *bytes.Buffer) string {
	t.Helper()
	var doc struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("error document does not parse: %v\n%s", err, out.String())
	}
	return doc.Error.Code + ": " + doc.Error.Message
}

func configRuntimeDir(c Config) string {
	_, dir := projectPaths(c)
	return dir
}

func TestConfigShowReportsSavedEffectiveAndController(t *testing.T) {
	c := repoConfig(t, "config")
	writeProfileFile(t, c.ProfilePath, `{"version":1,"agent":"claude","jobs":3,"x-custom":{"keep":true}}`)
	cc := headlessConfig(t, "config", "--show")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("show failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["kind"] != configShowKind || doc["scope"] != configScopeProject {
		t.Fatalf("document identity: %v %v", doc["kind"], doc["scope"])
	}
	saved := doc["saved"].(map[string]any)
	if saved["worker"] != "claude" || saved["review_every"] != nil {
		t.Fatalf("saved view: %#v", saved)
	}
	effective := doc["effective"].(map[string]any)
	worker := effective["worker"].(map[string]any)
	if worker["value"] != "claude" || worker["source"] != "profile" {
		t.Fatalf("effective worker: %#v", worker)
	}
	controller := doc["controller"].(map[string]any)
	if controller["state"] != "stopped" {
		t.Fatalf("controller: %#v", controller)
	}
	assertConfigSchema(t, &out)
}

// configShowText runs the plain-text show path and returns its lines.
func configShowText(t *testing.T, c Config) []string {
	t.Helper()
	var out, stderr bytes.Buffer
	if code := runConfig(c, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("show failed: %d %s", code, stderr.String())
	}
	return strings.Split(out.String(), "\n")
}

// hasLine reports whether one exact line appears, so a sentence glued to the
// preceding settings line cannot match.
func hasLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}
	return false
}

func TestConfigShowTextKeepsTheSettingsLineSeparate(t *testing.T) {
	c := repoConfig(t, "config", "--show")
	lines := configShowText(t, c)
	settings := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "agent ") {
			settings = line
		}
	}
	if settings == "" {
		t.Fatalf("no effective-settings line in %q", lines)
	}
	if !hasLine(lines, "Effective settings:") || !hasLine(lines, "No saved profile exists.") {
		t.Fatalf("labels must stay whole lines: %q", lines)
	}
	if !hasLine(lines, "No controller owns this repository; saved changes apply at the next start.") {
		t.Fatalf("the controller sentence ran into another line: %q", lines)
	}

	writeProfileFile(t, c.ProfilePath, `{"version":1,"agent":"claude","jobs":3}`)
	// Re-parse so the saved profile reaches the effective settings; a Config
	// parsed before the profile exists keeps its parse-time sources.
	saved, err := Parse([]string{"config", "--show"})
	if err != nil {
		t.Fatal(err)
	}
	lines = configShowText(t, saved)
	if hasLine(lines, "No saved profile exists.") {
		t.Fatalf("saved profile must drop the no-profile sentence: %q", lines)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "agent claude [profile]") {
		t.Fatalf("the saved profile must reach the effective settings: %q", lines)
	}
	if !hasLine(lines, "No controller owns this repository; saved changes apply at the next start.") {
		t.Fatalf("the controller sentence ran into another line: %q", lines)
	}
}

func TestConfigShowCreatesNothing(t *testing.T) {
	c := repoConfig(t, "config")
	cc := headlessConfig(t, "config", "--show")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("show failed: %d %s", code, stderr.String())
	}
	if _, err := os.Stat(configRuntimeDir(c)); !os.IsNotExist(err) {
		t.Fatal("show must not create the runtime directory")
	}
	if _, err := os.Stat(filepath.Join(c.Root, ".git", "looper.lock")); !os.IsNotExist(err) {
		t.Fatal("show must not create or probe the lock file")
	}
	// Machine show inside a repository reads the catalog only.
	cc = headlessConfig(t, "config", "--show", "--scope", "machine")
	out.Reset()
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("machine show failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["saved"] != nil || doc["effective"] != nil || doc["controller"] != nil {
		t.Fatalf("machine show must not report project state: %s", out.String())
	}
	if _, err := os.Stat(configRuntimeDir(c)); !os.IsNotExist(err) {
		t.Fatal("machine show must not create the runtime directory")
	}
	assertConfigSchema(t, &out)
}

func TestConfigPreviewValidatesAndWritesNothing(t *testing.T) {
	c := repoConfig(t, "config")
	profileJSON := "{\n  \"version\": 1,\n  \"agent\": \"claude\",\n  \"jobs\": 3\n}\n"
	writeProfileFile(t, c.ProfilePath, profileJSON)
	cc := headlessConfig(t, "config", "--set", "jobs=2", "--set", "review_every=0", "--preview")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("preview failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["kind"] != configPreviewKind {
		t.Fatalf("kind: %v", doc["kind"])
	}
	changes := doc["changes"].([]any)
	if len(changes) != 2 {
		t.Fatalf("changes: %#v", changes)
	}
	first := changes[0].(map[string]any)
	if first["field"] != "jobs" || first["to"] != float64(2) || first["outcome"] != configOutcomeSaved {
		t.Fatalf("jobs change: %#v", first)
	}
	application := doc["application"].(map[string]any)
	if application["mode"] != "stopped-file" {
		t.Fatalf("application: %#v", application)
	}
	if _, err := os.Stat(filepath.Join(configRuntimeDir(c), configJournalName)); !os.IsNotExist(err) {
		t.Fatal("preview must not write the settings journal")
	}
	after, err := os.ReadFile(c.ProfilePath)
	if err != nil || string(after) != profileJSON {
		t.Fatalf("preview must not change the profile: %v %s", err, after)
	}
	assertConfigSchema(t, &out)
}

func TestConfigPreviewFlagsOverriddenOutcome(t *testing.T) {
	repoConfig(t, "config")
	t.Setenv("LOOPER_JOBS", "4")
	cc, err := Parse([]string{"config", "--set", "jobs=2", "--preview"})
	if err != nil {
		t.Fatal(err)
	}
	cc = v2Config(cc, "config", true)
	if cc.Sources.Jobs != SourceEnv {
		t.Fatalf("environment source missing: %+v", cc.Sources)
	}
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("preview failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	change := doc["changes"].([]any)[0].(map[string]any)
	if change["outcome"] != configOutcomeOverridden {
		t.Fatalf("outcome: %#v", change)
	}
}

func TestConfigSetStoppedSavesAndJournals(t *testing.T) {
	c := repoConfig(t, "config")
	profileCatalogForTest(t, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{
		"claude-alt": {Adapter: "claude", Command: "/usr/local/bin/claude-alt"},
	}})
	writeProfileFile(t, c.ProfilePath, `{"version":1,"agent":"claude","x-custom":{"keep":true}}`)
	cc := headlessConfig(t, "config", "--set", "worker=claude-alt", "--set", "jobs=2")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("set failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["kind"] != configSavedKind {
		t.Fatalf("kind: %v", doc["kind"])
	}
	journal := doc["journal"].(map[string]any)
	if journal["state"] != "done" {
		t.Fatalf("journal: %#v", journal)
	}
	saved, err := LoadProfile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Agent != "claude-alt" || saved.Jobs != 2 {
		t.Fatalf("saved profile: %+v", saved)
	}
	if _, ok := saved.Unknown["x-custom"]; !ok {
		t.Fatal("unknown profile keys must survive a headless edit")
	}
	if doc["revision"] != profileDigest(c.ProfilePath) {
		t.Fatal("the saved document must report the stored profile digest")
	}
	j, err := loadConfigJournal(filepath.Join(configRuntimeDir(c), configJournalName))
	if err != nil || len(j.Entries) != 1 || j.Entries[0].State != "done" {
		t.Fatalf("journal file: %+v %v", j, err)
	}
	assertConfigSchema(t, &out)
}

func TestConfigSetStoppedIdempotentReplay(t *testing.T) {
	c := repoConfig(t, "config")
	first := headlessConfig(t, "config", "--set", "worker=claude", "--idempotency-key", "cfg-key-1")
	var out, stderr bytes.Buffer
	if code := runConfig(first, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("first set failed: %d %s", code, stderr.String())
	}
	out.Reset()
	second := headlessConfig(t, "config", "--set", "worker=claude", "--idempotency-key", "cfg-key-1")
	if code := runConfig(second, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("replay failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	detail := doc["detail"].(map[string]any)
	if !strings.Contains(detail["status"].(string), "replay") {
		t.Fatalf("replay detail: %#v", detail)
	}
	j, err := loadConfigJournal(filepath.Join(configRuntimeDir(c), configJournalName))
	if err != nil || len(j.Entries) != 1 {
		t.Fatalf("replay must not append journal entries: %+v %v", j, err)
	}
}

func TestConfigJournalResolvesAfterCrash(t *testing.T) {
	c := repoConfig(t, "config")
	// The intended save landed but the process died before the journal moved.
	oldProfile := "{\n  \"version\": 1,\n  \"agent\": \"claude\"\n}\n"
	newProfile := "{\n  \"version\": 1,\n  \"agent\": \"claude\",\n  \"jobs\": 2\n}\n"
	writeProfileFile(t, c.ProfilePath, newProfile)
	dir := configRuntimeDir(c)
	j := &configJournalFile{Entries: []configJournalEntry{{
		ID: "cfgset-crash", Scope: configScopeProject,
		OldDigest: digest([]byte(oldProfile)), NewDigest: digest([]byte(newProfile)),
		Patch: map[string]string{"jobs": "2"}, State: "prepared",
		CreatedAt: "2026-11-14T00:00:00Z", UpdatedAt: "2026-11-14T00:00:00Z",
	}}}
	if err := saveConfigJournal(filepath.Join(dir, configJournalName), j); err != nil {
		t.Fatal(err)
	}
	cc := headlessConfig(t, "config", "--set", "worker=claude")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("edit after crash failed: %d %s %s", code, stderr.String(), out.String())
	}
	resolved, err := loadConfigJournal(filepath.Join(dir, configJournalName))
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{}
	for _, e := range resolved.Entries {
		states[e.ID] = e.State
	}
	if states["cfgset-crash"] != "done" {
		t.Fatalf("the landed save must resolve to done: %+v", resolved.Entries)
	}

	// A foreign digest refuses instead of replacing the profile.
	writeProfileFile(t, c.ProfilePath, "{\n  \"version\": 1,\n  \"agent\": \"codex\"\n}\n")
	j.Entries = append(j.Entries, configJournalEntry{
		ID: "cfgset-foreign", Scope: configScopeProject,
		OldDigest: digest([]byte(newProfile)), NewDigest: digest([]byte("other" + "\n")),
		Patch: map[string]string{"jobs": "9"}, State: "prepared",
		CreatedAt: "2026-11-14T00:01:00Z", UpdatedAt: "2026-11-14T00:01:00Z",
	})
	if err := saveConfigJournal(filepath.Join(dir, configJournalName), j); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	cc = headlessConfig(t, "config", "--set", "worker=claude")
	if code := runConfig(cc, os.Stdin, &out, &stderr); code == 0 {
		t.Fatal("an unexpected profile digest must refuse the edit")
	}
	if text := configErrText(t, &out); !strings.Contains(text, "changed outside") {
		t.Fatalf("unexpected error %q", text)
	}
}

// An unset of a numeric project setting must behave like --unset jobs: the
// command succeeds, and review_every leaves the saved profile instead of
// failing numeric validation on the empty unset value.
func TestConfigUnsetNumericProjectSettings(t *testing.T) {
	c := repoConfig(t, "config")
	writeProfileFile(t, c.ProfilePath, `{"version":1,"agent":"claude","jobs":3,"review_every":5}`)

	// The preview reports the removal and writes nothing.
	cc := headlessConfig(t, "config", "--unset", "review_every", "--preview")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("preview unset review_every failed: %d %s %s", code, stderr.String(), out.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["kind"] != configPreviewKind {
		t.Fatalf("kind: %v", doc["kind"])
	}
	changes := doc["changes"].([]any)
	if len(changes) != 1 {
		t.Fatalf("changes: %#v", changes)
	}
	change := changes[0].(map[string]any)
	if change["field"] != "review_every" || change["from"] != float64(5) || change["to"] != nil {
		t.Fatalf("review_every removal: %#v", change)
	}
	assertConfigSchema(t, &out)
	if _, err := os.Stat(filepath.Join(configRuntimeDir(c), configJournalName)); !os.IsNotExist(err) {
		t.Fatal("preview must not write the settings journal")
	}
	saved, err := LoadProfile(c.ProfilePath)
	if err != nil || saved.ReviewEvery == nil || *saved.ReviewEvery != 5 {
		t.Fatalf("preview must not change the profile: %+v %v", saved, err)
	}

	// The plain unset removes review_every beside a jobs unset.
	out.Reset()
	cc = headlessConfig(t, "config", "--unset", "review_every", "--unset", "jobs")
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("unset failed: %d %s %s", code, stderr.String(), out.String())
	}
	doc = decodeConfigDoc(t, &out)
	if doc["kind"] != configSavedKind {
		t.Fatalf("kind: %v", doc["kind"])
	}
	assertConfigSchema(t, &out)
	saved, err = LoadProfile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReviewEvery != nil {
		t.Fatalf("unset review_every must remove the saved value: %+v", saved)
	}
	if saved.Jobs != 0 {
		t.Fatalf("unset jobs must clear jobs beside review_every: %+v", saved)
	}
	raw, err := os.ReadFile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "review_every") {
		t.Fatalf("the saved profile must drop the review_every key: %s", raw)
	}
	j, err := loadConfigJournal(filepath.Join(configRuntimeDir(c), configJournalName))
	if err != nil || len(j.Entries) != 1 || j.Entries[0].State != "done" {
		t.Fatalf("journal file: %+v %v", j, err)
	}
}

func TestConfigUnsetReviewerFollowsWorker(t *testing.T) {
	c := repoConfig(t, "config")
	writeProfileFile(t, c.ProfilePath, `{"version":1,"agent":"claude","review_agent":"codex"}`)
	// A machine reviewer default exists; unsetting must not bake it in.
	profileCatalogForTest(t, &AgentCatalog{Version: 1, Reviewer: "codex", Agents: map[string]AgentProfile{
		"codex": {Adapter: "codex", Command: "codex"},
	}})
	cc, err := Parse([]string{"config", "--unset", "reviewer"})
	if err != nil {
		t.Fatal(err)
	}
	cc = v2Config(cc, "config", true)
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("unset failed: %d %s", code, stderr.String())
	}
	saved, err := LoadProfile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReviewAgent != "" {
		t.Fatalf("unset reviewer must clear the saved value: %+v", saved)
	}
}

func TestConfigAliasesShareOnePayloadDigest(t *testing.T) {
	public, err := parseConfigPatch(configScopeProject, []string{"worker=claude", "reviewer=claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	internal, err := parseConfigPatch(configScopeProject, []string{"agent=claude", "review_agent=claude"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	publicFields, err := projectPatchFields(public)
	if err != nil {
		t.Fatal(err)
	}
	internalFields, err := projectPatchFields(internal)
	if err != nil {
		t.Fatal(err)
	}
	if requestPayloadDigest(&SettingsRequest{Type: RequestTypeSettings, Fields: publicFields}, nil) !=
		requestPayloadDigest(&SettingsRequest{Type: RequestTypeSettings, Fields: internalFields}, nil) {
		t.Fatal("alias assignments must produce one canonical payload digest")
	}
}

func TestConfigAssignmentParsing(t *testing.T) {
	if _, err := parseConfigPatch(configScopeProject, []string{"jobs=1", "jobs=2"}, nil); err == nil || !strings.Contains(err.Error(), "more than one --set or --unset") {
		t.Fatalf("duplicate assignment: %v", err)
	}
	if _, err := parseConfigPatch(configScopeProject, []string{"noequalssign"}, nil); err == nil || !strings.Contains(err.Error(), "=") {
		t.Fatalf("missing separator: %v", err)
	}
	if _, err := parseConfigPatch(configScopeProject, []string{"bogus=1"}, nil); err == nil || !strings.Contains(err.Error(), "not a project setting") {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := parseConfigPatch(configScopeProject, []string{"agents.glm.adapter=claude"}, nil); err == nil || !strings.Contains(err.Error(), "--scope machine") {
		t.Fatalf("project scope must refuse machine fields: %v", err)
	}
	if _, err := parseConfigPatch(configScopeMachine, []string{"jobs=2"}, nil); err == nil || !strings.Contains(err.Error(), "project profile") {
		t.Fatalf("machine scope must refuse project fields: %v", err)
	}
	// Values keep every character after the first equals sign.
	patch, err := parseConfigPatch(configScopeProject, []string{"verify_command=make check=1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if patch.fields["verify_command"] != "make check=1" {
		t.Fatalf("value mangled: %q", patch.fields["verify_command"])
	}
	// A dotted machine profile name parses by its final known suffix.
	patch, err = parseConfigPatch(configScopeMachine, []string{"agents.tool.fast.model=m1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if patch.fields["agents.tool.fast.model"] != "m1" {
		t.Fatalf("dotted name: %+v", patch.fields)
	}
	if _, err := parseConfigPatch(configScopeMachine, []string{"agents.tool.bogus=1"}, nil); err == nil || !strings.Contains(err.Error(), "suffix") {
		t.Fatalf("unknown agent suffix: %v", err)
	}
}

func TestConfigMachineDefineUpdateAndValidate(t *testing.T) {
	c := plainConfig(t, "config", "--scope", "machine")
	path := c.AgentCatalogPath

	// A new profile needs its adapter and command together.
	cc := headlessConfig(t, "config", "--scope", "machine", "--set", "agents.claude-alt.adapter=claude")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code == 0 {
		t.Fatal("a new profile without a command must fail")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("a refused patch must not create the catalog")
	}

	// Defining it works outside a repository and creates no project files.
	out.Reset()
	cc = headlessConfig(t, "config", "--scope", "machine",
		"--set", "agents.claude-alt.adapter=claude",
		"--set", "agents.claude-alt.command=/opt/tools/claude alt",
		"--set", "worker=claude-alt")
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("define failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["scope"] != configScopeMachine {
		t.Fatalf("scope: %v", doc["scope"])
	}
	assertConfigSchema(t, &out)
	catalog, err := loadAgentCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Worker != "claude-alt" || catalog.Agents["claude-alt"].Command != "/opt/tools/claude alt" {
		t.Fatalf("catalog: %+v", catalog)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "state.db")); !os.IsNotExist(err) {
		t.Fatal("a machine edit must not create project state")
	}

	// A partial update keeps the rest of the definition.
	out.Reset()
	cc = headlessConfig(t, "config", "--scope", "machine", "--set", "agents.claude-alt.model=fable-5")
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("update failed: %d %s", code, stderr.String())
	}
	catalog, err = loadAgentCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	p := catalog.Agents["claude-alt"]
	if p.Model != "fable-5" || p.Command != "/opt/tools/claude alt" {
		t.Fatalf("partial update: %+v", p)
	}

	// The complete catalog is validated: zcode has no effort override.
	out.Reset()
	cc = headlessConfig(t, "config", "--scope", "machine", "--set", "agents.claude-alt.effort=high", "--set", "agents.claude-alt.adapter=zcode")
	if code := runConfig(cc, os.Stdin, &out, &stderr); code == 0 {
		t.Fatal("an unsupported effort override must fail")
	}
	catalog, err = loadAgentCatalog(path)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.Agents["claude-alt"].Effort != "" || catalog.Agents["claude-alt"].Adapter != "claude" {
		t.Fatalf("a refused patch must not change the catalog: %+v", catalog.Agents["claude-alt"])
	}
}

func TestConfigMachineIfRevisionGuardsConcurrency(t *testing.T) {
	c := plainConfig(t, "config", "--scope", "machine")
	cc, err := Parse([]string{"config", "--scope", "machine", "--set", "agents.glm.adapter=claude", "--set", "agents.glm.command=claude", "--preview"})
	if err != nil {
		t.Fatal(err)
	}
	cc = v2Config(cc, "config", true)
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("preview failed: %d %s", code, stderr.String())
	}
	doc := decodeConfigDoc(t, &out)
	if doc["revision"] != "" {
		t.Fatalf("an absent catalog previews revision %v", doc["revision"])
	}

	// Another writer lands a change first; the stale revision must refuse.
	if err := saveAgentCatalog(c.AgentCatalogPath, &AgentCatalog{Version: 1, Agents: map[string]AgentProfile{
		"claude": {Adapter: "claude", Command: "claude"},
	}}); err != nil {
		t.Fatal(err)
	}
	current := profileDigest(c.AgentCatalogPath)
	stale := headlessConfig(t, "config", "--scope", "machine", "--set", "agents.glm.command=claude", "--if-revision", "0"+"000000000000000000000000000000000000000000000000000000000000000")
	out.Reset()
	if code := runConfig(stale, os.Stdin, &out, &stderr); code == 0 {
		t.Fatal("a stale revision must refuse the edit")
	}
	if text := configErrText(t, &out); !strings.Contains(text, "changed since the revision") {
		t.Fatalf("unexpected error %q", text)
	}

	fresh, err := Parse([]string{"config", "--scope", "machine", "--set", "agents.glm.adapter=claude", "--set", "agents.glm.command=claude", "--if-revision", current})
	if err != nil {
		t.Fatal(err)
	}
	fresh = v2Config(fresh, "config", true)
	out.Reset()
	if code := runConfig(fresh, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("matching revision failed: %d %s", code, stderr.String())
	}
	catalog, err := loadAgentCatalog(c.AgentCatalogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.Agents["glm"]; !ok {
		t.Fatalf("the guarded edit must land: %+v", catalog.Agents)
	}
}

func TestConfigMachineRejectsRequestOptions(t *testing.T) {
	plainConfig(t, "config")
	if _, err := Parse([]string{"config", "--scope", "machine", "--set", "worker=claude", "--wait"}); err == nil {
		t.Fatal("--wait must not combine with machine scope")
	} else if !strings.Contains(err.Error(), "project requests") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := Parse([]string{"config", "--scope", "machine", "--set", "worker=claude", "--idempotency-key", "k"}); err == nil {
		t.Fatal("--idempotency-key must not combine with machine scope")
	}
	if _, err := Parse([]string{"config", "--preview"}); err == nil {
		t.Fatal("--preview needs an assignment")
	} else if !strings.Contains(err.Error(), "at least one") {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := Parse([]string{"config", "--set", "worker=claude", "--unset", "worker"}); err == nil {
		t.Fatal("a field cannot be set and unset together")
	}
}

func TestConfigSetStoppedRefusesWaitWithoutController(t *testing.T) {
	c := repoConfig(t, "config")
	cc, err := Parse([]string{"config", "--set", "jobs=2", "--wait"})
	if err != nil {
		t.Fatal(err)
	}
	cc = v2Config(cc, "config", true)
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code == 0 {
		t.Fatal("--wait without a controller must refuse")
	}
	if text := configErrText(t, &out); !strings.Contains(text, "no controller owns this repository") {
		t.Fatalf("unexpected error %q", text)
	}
	if _, err := os.Stat(c.ProfilePath); !os.IsNotExist(err) {
		t.Fatal("the refused edit must not create a profile")
	}
}

func TestConfigLiveSubmitAppliesThroughController(t *testing.T) {
	c := repoConfig(t, "config")
	dir := configRuntimeDir(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	SaveProfile(c.ProfilePath, &Profile{Version: 1})
	// A live controller holds the lock and publishes metadata with this
	// process, which keeps the session verifiable.
	common := filepath.Join(c.Root, ".git")
	lock, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	info := &ControllerInfo{Version: 1, Session: "run-livecfg", PID: os.Getpid(), Mode: ControllerModeForeground}
	if err = writeJSON(controllerPath(dir), info); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{
		Dir: dir, Store: store, Config: c, Output: bytes.NewBuffer(nil),
		controller: &ControllerInfo{Version: 1, Session: "run-livecfg", PID: os.Getpid(), Mode: ControllerModeForeground},
		acks:       map[string]SettingsAck{},
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			e.processSettingsRequests()
			time.Sleep(10 * time.Millisecond)
		}
	}()

	cc, err := Parse([]string{"config", "--set", "worker=claude", "--set", "jobs=2", "--wait"})
	if err != nil {
		t.Fatal(err)
	}
	cc = v2Config(cc, "config", true)
	var out, stderr bytes.Buffer
	code := runConfig(cc, os.Stdin, &out, &stderr)
	wg.Wait()
	if code != 0 {
		t.Fatalf("live set failed: %d %s %s", code, stderr.String(), out.String())
	}
	saved, err := LoadProfile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Agent != "claude" || saved.Jobs != 2 {
		t.Fatalf("live application missed the profile: %+v", saved)
	}
	// A stopped-save journal never appears on the live path.
	if _, err := os.Stat(filepath.Join(dir, configJournalName)); !os.IsNotExist(err) {
		t.Fatal("the live path must not write the stopped-save journal")
	}
}

// The unset spellings stay distinct: review_every renders as the null
// removal the protocol applies, jobs keeps its stored auto-mode zero, and a
// numeric set never parses null as a value.
func TestProjectPatchFieldsRemovalSpelling(t *testing.T) {
	patch, err := parseConfigPatch(configScopeProject, nil, []string{"review_every", "jobs"})
	if err != nil {
		t.Fatal(err)
	}
	fields, err := projectPatchFields(patch)
	if err != nil {
		t.Fatal(err)
	}
	if string(fields["review_every"]) != "null" {
		t.Fatalf("unset review_every must render as null: %s", fields["review_every"])
	}
	if string(fields["jobs"]) != "0" {
		t.Fatalf("unset jobs must keep stored auto mode: %s", fields["jobs"])
	}
	patch, err = parseConfigPatch(configScopeProject, []string{"review_every=3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fields, err = projectPatchFields(patch)
	if err != nil {
		t.Fatal(err)
	}
	if string(fields["review_every"]) != "3" {
		t.Fatalf("a numeric set must render as a number: %s", fields["review_every"])
	}
	// The removal has exactly one spelling; --set review_every=null stays a
	// refused value, not a second path to the removal.
	patch, err = parseConfigPatch(configScopeProject, []string{"review_every=null"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = projectPatchFields(patch); err == nil || !strings.Contains(err.Error(), "nonnegative integer") {
		t.Fatalf("--set review_every=null must fail numeric validation: %v", err)
	}
}

// The live path now expresses the same per-field removal the stopped path
// saves: --unset review_every under a running controller submits
// review_every null, the controller drops the saved key instead of storing
// 0, and the acknowledgement names the next-run boundary (T154).
func TestConfigLiveUnsetReviewEveryRemovesSavedCadence(t *testing.T) {
	c := repoConfig(t, "config")
	dir := configRuntimeDir(c)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, c.ProfilePath, `{"version":1,"agent":"claude","review_every":5}`)
	// A live controller holds the lock and publishes metadata with this
	// process, which keeps the session verifiable.
	common := filepath.Join(c.Root, ".git")
	lock, err := os.OpenFile(filepath.Join(common, "looper.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	info := &ControllerInfo{Version: 1, Session: "run-livecfg-unset", PID: os.Getpid(), Mode: ControllerModeForeground}
	if err = writeJSON(controllerPath(dir), info); err != nil {
		t.Fatal(err)
	}

	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e := &Engine{
		Dir: dir, Store: store, Config: c, Output: bytes.NewBuffer(nil),
		controller: info,
		acks:       map[string]SettingsAck{},
	}

	cc := headlessConfig(t, "config", "--unset", "review_every")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code != 0 {
		t.Fatalf("live unset failed: %d %s %s", code, stderr.String(), out.String())
	}
	// The published envelope carries the removal, not a zero assignment.
	names := queuedJSONNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("one settings request must sit in the queue: %v", names)
	}
	envelopeRaw, err := os.ReadFile(filepath.Join(requestDir(dir), names[0]))
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Fields map[string]json.RawMessage `json:"fields"`
	}
	if err = json.Unmarshal(envelopeRaw, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope.Fields["review_every"]) != "null" {
		t.Fatalf("the live envelope must carry the null removal: %s", envelope.Fields["review_every"])
	}

	e.processSettingsRequests()
	if e.err != nil {
		t.Fatalf("controller failed on the removal: %v", e.err)
	}
	saved, err := LoadProfile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if saved.ReviewEvery != nil {
		t.Fatalf("the live removal must drop the saved key: %+v", saved)
	}
	rawProfile, err := os.ReadFile(c.ProfilePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawProfile), "review_every") {
		t.Fatalf("the saved profile must omit the review_every key: %s", rawProfile)
	}
	ack, ok := readRequestAck(dir, strings.TrimSuffix(names[0], ".json"))
	if !ok || ack.Status != AckApplied {
		t.Fatalf("the removal must apply: %+v %v", ack, ok)
	}
	if ack.Outcomes["review_every"] != "saved; applies on the next controller run" {
		t.Fatalf("outcome %q", ack.Outcomes["review_every"])
	}
	// The effective cadence keeps its snapshot; the default applies on the
	// next controller run, never mid-run.
	if e.Config.ReviewEvery != 5 {
		t.Fatalf("effective review cadence changed mid-run: %+v", e.Config)
	}
	// The stopped-save journal never appears on the live path.
	if _, err = os.Stat(filepath.Join(dir, configJournalName)); !os.IsNotExist(err) {
		t.Fatal("the live path must not write the stopped-save journal")
	}
}

func TestConfigProjectSetUnknownRoleRefuses(t *testing.T) {
	repoConfig(t, "config")
	cc := headlessConfig(t, "config", "--set", "worker=no-such-agent")
	var out, stderr bytes.Buffer
	if code := runConfig(cc, os.Stdin, &out, &stderr); code == 0 {
		t.Fatal("an unknown role must refuse before any write")
	}
	if text := configErrText(t, &out); !strings.Contains(text, "unknown agent profile") {
		t.Fatalf("unexpected error %q", text)
	}
}

// A current journal entry always records its unset list, even empty, so a
// key that recorded `--set agent=` refuses a later `--unset agent` instead of
// replaying it. Only a legacy entry without the field compares patches alone.
func TestConfigJournalKeepsSetEmptyAndUnsetDistinct(t *testing.T) {
	set, err := parseConfigPatch(configScopeProject, []string{"agent="}, nil)
	if err != nil {
		t.Fatal(err)
	}
	unset, err := parseConfigPatch(configScopeProject, nil, []string{"agent"})
	if err != nil {
		t.Fatal(err)
	}
	entry := configJournalEntry{ID: "cfgset-1", Key: "k", State: "done", Patch: configPatchMap(set), Unsets: configPatchUnsets(set)}
	path := filepath.Join(t.TempDir(), configJournalName)
	if err := saveConfigJournal(path, &configJournalFile{Entries: []configJournalEntry{entry}}); err != nil {
		t.Fatal(err)
	}
	j, err := loadConfigJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if j.Entries[0].Unsets == nil {
		t.Fatal("a current entry must round-trip a non-nil unset list")
	}
	if findConfigJournalReplay(j, "k", unset) != nil || findConfigJournalConflict(j, "k", unset) == nil {
		t.Fatal("an unset under a key that recorded set-empty must conflict, not replay")
	}
	if findConfigJournalReplay(j, "k", set) == nil {
		t.Fatal("the same set-empty edit must replay")
	}
	j.Entries[0].Unsets = nil
	if findConfigJournalReplay(j, "k", unset) == nil {
		t.Fatal("a legacy entry compares the patch map alone")
	}
}
