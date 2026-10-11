package looper

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProfileFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime", "profile.json")
	p := &Profile{Version: 1, Agent: "claude", Jobs: 4, ReviewEvery: func() *int { v := 3; return &v }(), Unknown: map[string]json.RawMessage{"custom": json.RawMessage(`{"kept":true}`)}}
	if err := SaveProfile(path, p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Agent != "claude" || got.Jobs != 4 || *got.ReviewEvery != 3 {
		t.Fatalf("values lost: %+v", got)
	}
	var kept map[string]any
	if err := json.Unmarshal(got.Unknown["custom"], &kept); err != nil || kept["kept"] != true {
		t.Fatalf("unknown key not preserved: %s", got.Unknown["custom"])
	}
	raw, _ := os.ReadFile(path)
	if !strings.Contains(string(raw), "\"version\": 1") {
		t.Fatalf("file not indented JSON: %s", raw)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("mode %v", info.Mode())
	}
}

func TestProfileDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	if p, err := LoadProfile(path); p != nil || err != nil {
		t.Fatalf("missing file must be no profile: %v %v", p, err)
	}
	writeProfileFile(t, path, `{`)
	if _, err := LoadProfile(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("malformed file must fail with the path: %v", err)
	}
	writeProfileFile(t, path, `{"version": 2}`)
	if _, err := LoadProfile(path); err == nil || !strings.Contains(err.Error(), "version 2") {
		t.Fatalf("unsupported version must fail: %v", err)
	}
	// A missing version key uses the default instead of failing.
	writeProfileFile(t, path, `{"agent": "codex"}`)
	p, err := LoadProfile(path)
	if err != nil || p == nil || p.Version != ProfileVersion || p.Agent != "codex" {
		t.Fatalf("missing version must default: %v %+v", err, p)
	}
	writeProfileFile(t, path, `{"version": 1, "agent": "gemini"}`)
	if _, err := LoadProfile(path); err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("invalid agent must fail: %v", err)
	}
	writeProfileFile(t, path, `{"version": 1, "jobs": -2}`)
	if _, err := LoadProfile(path); err == nil {
		t.Fatal("negative jobs accepted")
	}
	writeProfileFile(t, path, `{"version": 1, "jobs": "three"}`)
	if _, err := LoadProfile(path); err == nil {
		t.Fatal("string jobs accepted")
	}
	writeProfileFile(t, path, `{"version": 1, "agent": 5}`)
	if _, err := LoadProfile(path); err == nil {
		t.Fatal("number agent accepted")
	}
	writeProfileFile(t, path, `{"version": 1, "review_every": -1}`)
	if _, err := LoadProfile(path); err == nil {
		t.Fatal("negative review_every accepted")
	}
	// Null never silently becomes a zero value: review_every null must not
	// disable reviews, and jobs null must not become auto mode.
	writeProfileFile(t, path, `{"version": 1, "review_every": null}`)
	if _, err := LoadProfile(path); err == nil || !strings.Contains(err.Error(), "review_every") {
		t.Fatalf("null review_every accepted: %v", err)
	}
	writeProfileFile(t, path, `{"version": 1, "jobs": null}`)
	if _, err := LoadProfile(path); err == nil || !strings.Contains(err.Error(), "jobs") {
		t.Fatalf("null jobs accepted: %v", err)
	}
	writeProfileFile(t, path, `{"version": null}`)
	if _, err := LoadProfile(path); err == nil {
		t.Fatal("null version accepted")
	}
	// The invalid file must still exist untouched.
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if err := (&Profile{Version: 1, Agent: "bad"}).Validate(); err == nil {
		t.Fatal("validate accepted bad agent")
	}
}

func TestProfileMergePrecedence(t *testing.T) {
	clearSettingEnv(t)
	root := t.TempDir()
	base := filepath.Join(root, "base")
	t.Setenv("LOOPER_BASE_DIR", base)
	old, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	profilePath := ProfilePath(Config{Root: root, BaseDir: base})
	writeProfileFile(t, profilePath, `{"version":1,"agent":"claude","review_agent":"codex","jobs":3,"model":"profile-model","claude_model":"profile-claude","effort":"high","verify_command":"make check","setup_command":"make setup","review_every":2}`)

	// The profile supplies every setting when nothing else is present.
	c, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "codex" || c.Jobs != 3 || c.JobsAuto {
		t.Fatalf("profile merge broken: %+v", c)
	}
	if c.Model != "profile-model" || c.ClaudeModel != "profile-claude" || c.Effort != "high" {
		t.Fatalf("profile models broken: %+v", c)
	}
	if c.Verify != "make check" || c.Setup != "make setup" || c.ReviewEvery != 2 {
		t.Fatalf("profile commands broken: %+v", c)
	}
	if c.Sources.Agent != SourceProfile || c.Sources.Verify != SourceProfile {
		t.Fatalf("sources not recorded: %+v", c.Sources)
	}

	// An explicit flag equal to the built-in default still overrides the profile.
	c, err = Parse([]string{"--agent", "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "codex" || c.Sources.Agent != SourceFlag {
		t.Fatalf("flag equal to default must override the profile: %s %v", c.Agent, c.Sources.Agent)
	}
	if c.ReviewAgent != "codex" || c.Sources.ReviewAgent != SourceProfile {
		t.Fatalf("review agent must stay from the profile: %s %v", c.ReviewAgent, c.Sources.ReviewAgent)
	}

	// The environment sits between flags and the profile.
	t.Setenv("CODEX_MODEL", "env-model")
	c, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "env-model" || c.Sources.Model != SourceEnv {
		t.Fatalf("env precedence broken: %s %v", c.Model, c.Sources.Model)
	}
	// An explicitly empty environment variable counts as present.
	t.Setenv("CODEX_MODEL", "")
	c, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "" || c.Sources.Model != SourceEnv {
		t.Fatalf("empty env must override the profile: %q %v", c.Model, c.Sources.Model)
	}

	// Jobs zero stores auto mode.
	writeProfileFile(t, profilePath, `{"version":1,"jobs":0}`)
	c, err = Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !c.JobsAuto || c.Jobs < 1 {
		t.Fatalf("profile auto jobs broken: %d %v", c.Jobs, c.JobsAuto)
	}
}

func TestProfileGatesCommands(t *testing.T) {
	clearSettingEnv(t)
	root := t.TempDir()
	base := filepath.Join(root, "base")
	t.Setenv("LOOPER_BASE_DIR", base)
	old, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	profilePath := ProfilePath(Config{Root: root, BaseDir: base})
	writeProfileFile(t, profilePath, `not json`)

	if _, err := Parse(nil); err == nil {
		t.Fatal("controller command must fail on an invalid profile")
	}
	if _, err := Parse([]string{"status"}); err != nil {
		t.Fatalf("status must ignore the invalid profile: %v", err)
	}
	if _, err := Parse([]string{"--ls", "done"}); err != nil {
		t.Fatalf("status filtering must ignore the invalid profile: %v", err)
	}
	if _, err := Parse([]string{"stop"}); err != nil {
		t.Fatalf("stop must ignore the invalid profile: %v", err)
	}
}

func TestProfileDigestDetectsEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	if profileDigest(path) != "" {
		t.Fatal("missing profile must digest empty")
	}
	writeProfileFile(t, path, `{"version":1}`)
	first := profileDigest(path)
	writeProfileFile(t, path, `{"version":1,"jobs":2}`)
	if profileDigest(path) == first {
		t.Fatal("digest must change with content")
	}
}

func TestInterleaveWithProfile(t *testing.T) {
	clearSettingEnv(t)
	root := t.TempDir()
	base := filepath.Join(root, "base")
	t.Setenv("LOOPER_BASE_DIR", base)
	old, _ := os.Getwd()
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
	profilePath := ProfilePath(Config{Root: root, BaseDir: base})
	// A saved review agent is an explicit choice; --interleave keeps it.
	writeProfileFile(t, profilePath, `{"version":1,"review_agent":"claude"}`)
	c, err := Parse([]string{"--interleave"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Agent != "claude" || c.ReviewAgent != "claude" {
		t.Fatalf("interleave must respect the saved review agent: %s/%s", c.Agent, c.ReviewAgent)
	}
	// An empty review agent value receives the codex preset at the interleave
	// position, exactly as the original parser did; only a later flag wins.
	writeProfileFile(t, profilePath, `{"version":1}`)
	t.Setenv("LOOPER_REVIEW_AGENT", "")
	c, err = Parse([]string{"--interleave"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewAgent != "codex" {
		t.Fatalf("empty review agent takes the codex preset: %s", c.ReviewAgent)
	}
	// A later explicit empty flag clears the preset and follows the agent.
	c, err = Parse([]string{"--interleave", "--review-agent", ""})
	if err != nil {
		t.Fatal(err)
	}
	if c.ReviewAgent != "claude" {
		t.Fatalf("later empty review agent must follow the agent: %s", c.ReviewAgent)
	}
}

func TestSaveProfileExclusiveNeverReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.json")
	first := &Profile{Version: 1, Agent: "codex"}
	created, err := saveProfileExclusive(path, first)
	if err != nil || !created {
		t.Fatalf("first save failed: %v %v", created, err)
	}
	// A second racing first run keeps the earlier answers.
	created, err = saveProfileExclusive(path, &Profile{Version: 1, Agent: "claude"})
	if err != nil || created {
		t.Fatalf("second save must not replace: %v %v", created, err)
	}
	kept, err := LoadProfile(path)
	if err != nil || kept.Agent != "codex" {
		t.Fatalf("earlier answers were lost: %v %+v", err, kept)
	}
	// No temporary file survives the attempt.
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Fatal("temporary file survived")
	}
}
