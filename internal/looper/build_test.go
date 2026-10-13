package looper

import (
	"bytes"
	"encoding/json"
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
