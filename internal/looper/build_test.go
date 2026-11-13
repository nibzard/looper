package looper

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionJSONIdentifiesBuild(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := Main([]string{"--version", "--json"}, &out, &stderr); code != 0 {
		t.Fatalf("version: %d %s", code, &stderr)
	}
	var info map[string]any
	if err := json.Unmarshal(out.Bytes(), &info); err != nil {
		t.Fatalf("version is not structured: %s", &out)
	}
	if info["release"] != Version || info["revision"] == "" || info["revision"] == nil {
		t.Fatalf("missing build identity: %v", info)
	}
}

func TestExplicitStampsWinOverEmbeddedMetadata(t *testing.T) {
	revision, modified := BuildRevision, BuildModified
	t.Cleanup(func() { BuildRevision, BuildModified = revision, modified })

	// An explicit unknown is a statement, not a gap for embedded VCS metadata
	// to fill: an archive build keeps its unknown identity even when Go
	// stamped a revision from an enclosing repository.
	BuildRevision, BuildModified = "unknown", "false"
	if build := currentBuild(); build.Revision != "unknown" || build.Modified {
		t.Fatalf("explicit unknown stamp was overridden: %+v", build)
	}

	BuildRevision, BuildModified = "cafebabecafebabecafebabecafebabe", "true"
	if build := currentBuild(); build.Revision != "cafebabecafebabecafebabecafebabe" || !build.Modified {
		t.Fatalf("explicit stamps were overridden: %+v", build)
	}

	// Without stamps the embedded metadata, whatever this test binary
	// carries, decides. The revision is always stated, never blank.
	BuildRevision, BuildModified = "", ""
	if build := currentBuild(); build.Revision == "" {
		t.Fatalf("unstamped build hides its revision: %+v", build)
	}
}

func TestBuildSummaryTruncatesRevision(t *testing.T) {
	long := strings.Repeat("a", 40)
	summary := BuildInfo{Release: "9.9.9", Revision: long}.String()
	if summary != "looper 9.9.9 ("+long[:12]+")" {
		t.Fatalf("the summary must abbreviate the revision: %q", summary)
	}
	summary = BuildInfo{Release: "9.9.9", Revision: long, Modified: true}.String()
	if !strings.HasSuffix(summary, long[:12]+", modified)") {
		t.Fatalf("the summary must keep the modified marker: %q", summary)
	}
}

func TestControllerRetainsBuildIdentity(t *testing.T) {
	s := timingStore(t)
	info, err := publishController(t.TempDir(), ControllerModeForeground, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.beginSession(info); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err = s.db.QueryRow("SELECT data FROM events WHERE kind='session' ORDER BY id DESC LIMIT 1").Scan(&raw); err != nil {
		t.Fatalf("no durable build identity: %v", err)
	}
	var saved struct {
		Session string         `json:"session"`
		Build   map[string]any `json:"build"`
	}
	if err = json.Unmarshal([]byte(raw), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Session != info.Session || saved.Build["release"] != Version || saved.Build["revision"] == nil {
		t.Fatalf("incorrect session build: %+v", saved)
	}
}
