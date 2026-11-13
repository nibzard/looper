package contractschema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The contract artifacts live beside the Go packages so any worktree can use
// them without a network fetch or a generated copy.
const contractsRoot = "../../docs/contracts"

func loadJSON(t *testing.T, path string) any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var v any
	if err = json.Unmarshal(b, &v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return v
}

func TestValidatorAcceptsAndRejects(t *testing.T) {
	cases := []struct {
		name     string
		schema   string
		instance string
		accept   bool
	}{
		{"type object", `{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":"x"}`, true},
		{"type mismatch", `{"type":"object"}`, `[1,2]`, false},
		{"nullable array", `{"type":["array","null"]}`, `null`, true},
		{"integer field", `{"type":"integer"}`, `3`, true},
		{"number accepts integer", `{"type":"number"}`, `3`, true},
		{"integer rejects fraction", `{"type":"integer"}`, `3.5`, false},
		{"required missing", `{"type":"object","required":["a"]}`, `{}`, false},
		{"unknown property", `{"type":"object","properties":{},"additionalProperties":false}`, `{"b":1}`, false},
		{"additional schema", `{"type":"object","properties":{"a":{"type":"string"}},"additionalProperties":{"type":"integer"}}`, `{"a":"x","b":2}`, true},
		{"additional schema mismatch", `{"type":"object","properties":{},"additionalProperties":{"type":"integer"}}`, `{"b":"x"}`, false},
		{"enum", `{"enum":["a","b"]}`, `"b"`, true},
		{"enum miss", `{"enum":["a","b"]}`, `"c"`, false},
		{"const", `{"const":2}`, `2`, true},
		{"const miss", `{"const":2}`, `3`, false},
		{"minimum", `{"type":"integer","minimum":1}`, `1`, true},
		{"exclusive minimum", `{"type":"integer","exclusiveMinimum":0}`, `0`, false},
		{"maximum", `{"type":"integer","maximum":5}`, `6`, false},
		{"min length", `{"type":"string","minLength":2}`, `"ab"`, true},
		{"max length", `{"type":"string","maxLength":1}`, `"ab"`, false},
		{"pattern", `{"type":"string","pattern":"^[a-z0-9-]+$"}`, `"a-b"`, true},
		{"pattern miss", `{"type":"string","pattern":"^[a-z0-9-]+$"}`, `"A B"`, false},
		{"items", `{"type":"array","items":{"type":"integer"}}`, `[1,2]`, true},
		{"items miss", `{"type":"array","items":{"type":"integer"}}`, `[1,"x"]`, false},
		{"max items", `{"type":"array","maxItems":1}`, `[1,2]`, false},
		{"unique items", `{"type":"array","uniqueItems":true}`, `[1,1]`, false},
		{"min properties", `{"type":"object","minProperties":1}`, `{"a":1}`, true},
		{"oneOf match", `{"oneOf":[{"type":"string"},{"type":"integer","minimum":10}]}`, `12`, true},
		{"oneOf both", `{"oneOf":[{"type":"integer"},{"type":"number"}]}`, `1`, false},
		{"anyOf none", `{"anyOf":[{"type":"string"}]}`, `1`, false},
		{"allOf", `{"allOf":[{"type":"integer"},{"minimum":3}]}`, `4`, true},
		{"not", `{"not":{"type":"null"}}`, `1`, true},
		{"not violated", `{"not":{"type":"integer"}}`, `1`, false},
		{"local ref", `{"$defs":{"s":{"type":"string"}},"$ref":"#/$defs/s"}`, `"x"`, true},
		{"local ref miss", `{"$defs":{"s":{"type":"string"}},"$ref":"#/$defs/s"}`, `1`, false},
		{"unknown ref", `{"$ref":"#/definitions/missing"}`, `1`, false},
		{"ignored keywords", `{"title":"t","description":"d","default":1,"examples":[1],"format":"date-time","$schema":"x","$id":"y","type":"integer"}`, `1`, true},
	}
	for _, tc := range cases {
		var schema, instance any
		if err := json.Unmarshal([]byte(tc.schema), &schema); err != nil {
			t.Fatalf("%s: schema does not parse: %v", tc.name, err)
		}
		if err := json.Unmarshal([]byte(tc.instance), &instance); err != nil {
			t.Fatalf("%s: instance does not parse: %v", tc.name, err)
		}
		err := Validate(schema, instance)
		if tc.accept && err != nil {
			t.Errorf("%s: unexpected rejection: %v", tc.name, err)
		}
		if !tc.accept && err == nil {
			t.Errorf("%s: unexpected acceptance", tc.name)
		}
	}
}

// Every contract-version-2 fixture must validate against the schema named by
// its directory, and every invalid fixture must be rejected. A schema without
// both kinds of fixture is itself an error, so a new contract cannot land
// without examples that pin its rules.
func TestVersionTwoFixturesMatchSchemas(t *testing.T) {
	schemaDir := filepath.Join(contractsRoot, "schemas", "v2")
	entries, err := os.ReadDir(schemaDir)
	if err != nil {
		t.Skipf("contract schemas are absent: %v", err)
	}
	schemas := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".schema.json") {
			continue
		}
		schemas++
		name := strings.TrimSuffix(entry.Name(), ".schema.json")
		schema := loadJSON(t, filepath.Join(schemaDir, entry.Name()))
		for _, kind := range []string{"valid", "invalid"} {
			dir := filepath.Join(contractsRoot, "fixtures", "v2", name, kind)
			files, err := os.ReadDir(dir)
			if err != nil || len(files) == 0 {
				t.Errorf("%s has no %s fixtures", entry.Name(), kind)
				continue
			}
			for _, f := range files {
				if f.IsDir() {
					continue
				}
				path := filepath.Join(dir, f.Name())
				instance := loadJSON(t, path)
				err := Validate(schema, instance)
				switch {
				case kind == "valid" && err != nil:
					t.Errorf("%s must validate: %v", path, err)
				case kind == "invalid" && err == nil:
					t.Errorf("%s must be rejected", path)
				}
			}
		}
	}
	if schemas == 0 {
		t.Fatal("no contract schemas found")
	}
}

func TestExitCodeRegistry(t *testing.T) {
	path := filepath.Join(contractsRoot, "exit-codes.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("exit code registry is absent: %v", err)
	}
	doc := loadJSON(t, path).(map[string]any)
	codes, ok := doc["transport_exit_codes"].([]any)
	if !ok || len(codes) == 0 {
		t.Fatal("the registry lists no transport exit codes")
	}
	seen := map[int]bool{}
	names := map[string]bool{}
	for _, entry := range codes {
		c := entry.(map[string]any)
		code := int(c["code"].(float64))
		if code < 0 || code > 125 {
			t.Errorf("exit code %d is outside the portable range", code)
		}
		if seen[code] {
			t.Errorf("exit code %d is listed twice", code)
		}
		seen[code] = true
		name, _ := c["name"].(string)
		if name == "" {
			t.Errorf("exit code %d has no name", code)
		}
		if names[name] {
			t.Errorf("exit code name %q is used twice", name)
		}
		names[name] = true
		for _, field := range []string{"meaning", "retry", "legacy_equivalent"} {
			if _, ok := c[field]; !ok {
				t.Errorf("exit code %d is missing %s", code, field)
			}
		}
		switch c["retry"] {
		case "yes", "no", "after-change":
		default:
			t.Errorf("exit code %d has an invalid retry value %v", code, c["retry"])
		}
	}
	for _, required := range []int{0, 1, 2} {
		if !seen[required] {
			t.Errorf("the portable base exit code %d is missing", required)
		}
	}
}

// The request lifecycle registry, the receipt schema, and the record schema
// must name the same states. Keeping one list in three places invites drift,
// so this check compares them directly.
func TestRequestStateRegistryMatchesSchemas(t *testing.T) {
	path := filepath.Join(contractsRoot, "request-states.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("request state registry is absent: %v", err)
	}
	doc := loadJSON(t, path).(map[string]any)
	entries, ok := doc["states"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatal("the registry lists no request states")
	}
	states := map[string]bool{}
	terminal := map[string]bool{}
	for _, entry := range entries {
		s := entry.(map[string]any)
		name, _ := s["name"].(string)
		if name == "" {
			t.Fatal("a request state has no name")
		}
		if states[name] {
			t.Errorf("request state %q is listed twice", name)
		}
		states[name] = true
		terminal[name] = s["terminal"] == true
		if _, ok := s["meaning"]; !ok {
			t.Errorf("request state %q has no meaning", name)
		}
		if _, ok := s["writer"]; !ok {
			t.Errorf("request state %q names no writer", name)
		}
	}
	for _, schemaName := range []string{"request-receipt", "request-record"} {
		schemaPath := filepath.Join(contractsRoot, "schemas", "v2", schemaName+".schema.json")
		schema := loadJSON(t, schemaPath).(map[string]any)
		enum := findEnum(schema, "state")
		if len(enum) == 0 {
			t.Fatalf("%s does not enumerate request states", schemaName)
		}
		for _, name := range enum {
			if !states[name] {
				t.Errorf("%s lists state %q that the registry does not define", schemaName, name)
			}
		}
		for name := range states {
			found := false
			for _, listed := range enum {
				if listed == name {
					found = true
				}
			}
			if !found {
				t.Errorf("%s is missing registered state %q", schemaName, name)
			}
		}
	}
	if !terminal["applied"] || !terminal["rejected"] || !terminal["failed"] || !terminal["cancelled"] {
		t.Error("a terminal request state is marked non-terminal")
	}
	if terminal["submitted"] || terminal["accepted"] || terminal["waiting"] {
		t.Error("a non-terminal request state is marked terminal")
	}
}

// findEnum walks a schema document for the enum of the named property. Only
// the first match is used, which is enough for the request-state cross-check.
func findEnum(schema any, property string) []string {
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			if props, ok := t["properties"].(map[string]any); ok {
				if p, ok := props[property].(map[string]any); ok {
					if list, ok := p["enum"].([]any); ok {
						for _, item := range list {
							if s, ok := item.(string); ok {
								out = append(out, s)
							}
						}
					}
				}
			}
			for _, child := range t {
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(schema)
	return out
}

func TestShapeIgnoresValuesAndRecordsStructure(t *testing.T) {
	document := `{"a":"one","b":2,"c":[1,2],"d":[{"x":1},{"x":2,"y":3}],"e":null}`
	shape, err := ShapeJSON([]byte(document))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"a": "string",
		"b": "integer",
		"c": []any{"integer"},
		"d": []any{
			map[string]any{"x": "integer"},
			map[string]any{"x": "integer", "y": "integer"},
		},
		"e": "null",
	}
	if !ShapeEqual(shape, want) {
		t.Fatalf("shape mismatch:\n got %s\nwant %s", marshal(shape), marshal(want))
	}
	same, err := ShapeJSON([]byte(`{"a":"two","b":3,"c":[9],"d":[{"x":8},{"y":7,"x":6}],"e":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if !ShapeEqual(shape, same) {
		t.Error("equal structure with different values must compare equal")
	}
	changed, err := ShapeJSON([]byte(`{"a":"one","b":2,"c":[1,2],"d":[{"x":1},{"x":2,"y":3}],"e":null,"f":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if ShapeEqual(shape, changed) {
		t.Error("an added field must change the shape")
	}
	if _, err := ShapeJSON([]byte(document + `,"f":true}`)); err == nil {
		t.Error("trailing data after the document must be rejected")
	}
	empty, err := ShapeJSON([]byte(`{"c":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := empty.(map[string]any)["c"]; got != "empty array" {
		t.Errorf("empty array shape: %v", got)
	}
	// Distinct array item shapes stay in a stable order so recorded fixtures
	// do not depend on map iteration.
	for i := 0; i < 20; i++ {
		again, err := ShapeJSON([]byte(document))
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(again) != fmt.Sprint(shape) {
			t.Fatal("shape rendering is not deterministic")
		}
	}
	sort.Strings(nil)
}
