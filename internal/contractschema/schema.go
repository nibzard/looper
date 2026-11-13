// Package contractschema validates the JSON documents Looper publishes as
// agent contracts. The schemas under docs/contracts use a restricted JSON
// Schema subset so the repository needs no external validator dependency.
//
// Supported keywords: type (string or array), properties, required,
// additionalProperties, items, enum, const, minimum, maximum,
// exclusiveMinimum, exclusiveMaximum, minLength, maxLength, minItems,
// maxItems, uniqueItems, minProperties, oneOf, anyOf, allOf, not, and local
// $ref into $defs. The $schema, $id, title, description, default, examples,
// and format keywords are accepted and ignored; format checking stays a
// documentation concern.
package contractschema

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

// Validate checks one decoded JSON instance against one decoded schema. Both
// arguments must come from encoding/json decoding; numbers keep their
// json.Number form when the caller used a decoder with UseNumber.
func Validate(schema, instance any) error {
	root, ok := schema.(map[string]any)
	if !ok {
		return fmt.Errorf("the schema document is not a JSON object")
	}
	v := &validator{root: root}
	v.apply(root, canonical(instance), "$")
	if len(v.errs) > 0 {
		return fmt.Errorf("contract violation: %s", strings.Join(v.errs, "; "))
	}
	return nil
}

// ValidateJSON decodes both documents and validates the instance. It reports
// decoding problems separately from contract violations.
func ValidateJSON(schemaBytes, instanceBytes []byte) error {
	var schema, instance any
	if err := decode(schemaBytes, &schema); err != nil {
		return fmt.Errorf("the schema document is not valid JSON: %w", err)
	}
	if err := decode(instanceBytes, &instance); err != nil {
		return fmt.Errorf("the instance is not valid JSON: %w", err)
	}
	return Validate(schema, instance)
}

// decode reads exactly one JSON document. Trailing data is an error so a
// fixture or output stream cannot pass by ignoring what follows the document.
func decode(b []byte, into *any) error {
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	if err := d.Decode(into); err != nil {
		return err
	}
	if d.More() {
		return fmt.Errorf("trailing data after the JSON document")
	}
	return nil
}

type validator struct {
	root map[string]any
	errs []string
}

// valid reports whether a subschema accepts the instance. Subschemas used for
// combinator testing must not pollute the parent error list.
func valid(root, schema, instance any) bool {
	sub := &validator{root: rootMap(root)}
	sub.apply(schema, instance, "$")
	return len(sub.errs) == 0
}

func rootMap(schema any) map[string]any {
	if m, ok := schema.(map[string]any); ok {
		return m
	}
	return nil
}

func (v *validator) errorf(path, format string, args ...any) {
	v.errs = append(v.errs, fmt.Sprintf("%s: %s", path, fmt.Sprintf(format, args...)))
}

func (v *validator) apply(schema, instance any, path string) {
	s, ok := schema.(map[string]any)
	if !ok {
		v.errorf(path, "the schema entry is not an object")
		return
	}
	if ref, ok := s["$ref"].(string); ok && ref != "" {
		resolved, err := v.resolve(ref)
		if err != nil {
			v.errorf(path, "%s", err)
			return
		}
		v.apply(resolved, instance, path)
		return
	}
	if t, ok := s["type"]; ok && !v.checkType(t, instance, path) {
		return
	}
	if c, ok := s["const"]; ok && !equal(canonical(c), instance) {
		v.errorf(path, "value must equal %s", marshal(c))
	}
	if e, ok := s["enum"].([]any); ok && !anyEqual(e, instance) {
		v.errorf(path, "value must be one of %s", marshal(e))
	}
	if num, ok := number(instance); ok {
		v.checkBounds(s, num, path)
	}
	if str, ok := instance.(string); ok {
		v.checkStringLength(s, str, path)
	}
	if arr, ok := instance.([]any); ok {
		v.checkArray(s, arr, path)
	}
	if obj, ok := instance.(map[string]any); ok {
		v.checkObject(s, obj, path)
	}
	for _, combinator := range []string{"oneOf", "anyOf", "allOf"} {
		list, ok := s[combinator].([]any)
		if !ok {
			continue
		}
		matches := 0
		for _, sub := range list {
			if valid(v.root, sub, instance) {
				matches++
			}
		}
		switch combinator {
		case "oneOf":
			if matches != 1 {
				v.errorf(path, "matches %d subschemas of oneOf, want exactly 1", matches)
			}
		case "anyOf":
			if matches == 0 {
				v.errorf(path, "matches no subschema of anyOf")
			}
		case "allOf":
			if matches != len(list) {
				v.errorf(path, "matches %d of %d allOf subschemas", matches, len(list))
			}
		}
	}
	if sub, ok := s["not"]; ok && valid(v.root, sub, instance) {
		v.errorf(path, "matches a schema that forbids it")
	}
}

func (v *validator) resolve(ref string) (any, error) {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil, fmt.Errorf("unsupported $ref %q; only local #/$defs/ references are supported", ref)
	}
	name := strings.TrimPrefix(ref, prefix)
	defs, ok := v.root["$defs"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the schema has no $defs for %q", ref)
	}
	target, ok := defs[name]
	if !ok {
		return nil, fmt.Errorf("unknown $ref %q", ref)
	}
	return target, nil
}

// jsonKind reports the JSON type name of a canonicalized value.
func jsonKind(instance any) string {
	switch t := instance.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case string:
		return "string"
	case json.Number:
		if _, err := t.Int64(); err == nil {
			return "integer"
		}
		return "number"
	case int, int64:
		return "integer"
	case float64:
		if t == float64(int64(t)) {
			return "integer"
		}
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	}
	return fmt.Sprintf("%T", instance)
}

func (v *validator) checkType(spec, instance any, path string) bool {
	names, ok := spec.([]any)
	if !ok {
		names = []any{spec}
	}
	allowed := make([]string, 0, len(names))
	for _, name := range names {
		s, ok := name.(string)
		if !ok {
			v.errorf(path, "the schema type entry %v is not a string", name)
			return false
		}
		allowed = append(allowed, s)
		if typeMatches(s, instance) {
			return true
		}
	}
	v.errorf(path, "is %s, want %s", jsonKind(instance), strings.Join(allowed, " or "))
	return false
}

func typeMatches(name string, instance any) bool {
	kind := jsonKind(instance)
	if name == "number" {
		return kind == "number" || kind == "integer"
	}
	return name == kind
}

func (v *validator) checkBounds(s map[string]any, num float64, path string) {
	check := func(key string, ok func(float64) bool) {
		want, exists := s[key]
		if !exists {
			return
		}
		w, isNumber := number(canonical(want))
		if isNumber && !ok(w) {
			v.errorf(path, "%v is outside the bound %s %v", num, key, want)
		}
	}
	check("minimum", func(w float64) bool { return num >= w })
	check("maximum", func(w float64) bool { return num <= w })
	check("exclusiveMinimum", func(w float64) bool { return num > w })
	check("exclusiveMaximum", func(w float64) bool { return num < w })
}

func (v *validator) checkStringLength(s map[string]any, str string, path string) {
	if n, ok := integer(s["minLength"]); ok && len(str) < int(n) {
		v.errorf(path, "is shorter than minLength %d", n)
	}
	if n, ok := integer(s["maxLength"]); ok && len(str) > int(n) {
		v.errorf(path, "is longer than maxLength %d", n)
	}
	if p, ok := s["pattern"].(string); ok {
		re, err := regexp.Compile(p)
		if err != nil {
			v.errorf(path, "the schema pattern %q does not compile", p)
			return
		}
		if !re.MatchString(str) {
			v.errorf(path, "does not match pattern %q", p)
		}
	}
}

func (v *validator) checkArray(s map[string]any, arr []any, path string) {
	if n, ok := integer(s["minItems"]); ok && len(arr) < int(n) {
		v.errorf(path, "has %d items, want at least %d", len(arr), n)
	}
	if n, ok := integer(s["maxItems"]); ok && len(arr) > int(n) {
		v.errorf(path, "has %d items, want at most %d", len(arr), n)
	}
	if unique, ok := s["uniqueItems"].(bool); ok && unique && !uniqueValues(arr) {
		v.errorf(path, "has duplicate items")
	}
	itemSchema, hasItems := s["items"]
	if !hasItems {
		return
	}
	if prefix, ok := itemSchema.([]any); ok {
		// Draft 2020-12 dropped additionalItems; a tuple form here is an error
		// in the schema itself.
		v.errorf(path, "tuple items are not supported; use a single item schema")
		_ = prefix
		return
	}
	for i, item := range arr {
		v.apply(itemSchema, item, fmt.Sprintf("%s[%d]", path, i))
	}
}

func (v *validator) checkObject(s map[string]any, obj map[string]any, path string) {
	if list, ok := s["required"].([]any); ok {
		missing := []string{}
		for _, name := range list {
			if key, ok := name.(string); ok {
				if _, present := obj[key]; !present {
					missing = append(missing, key)
				}
			}
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			v.errorf(path, "is missing required properties: %s", strings.Join(missing, ", "))
		}
	}
	if n, ok := integer(s["minProperties"]); ok && len(obj) < int(n) {
		v.errorf(path, "has %d properties, want at least %d", len(obj), n)
	}
	props, _ := s["properties"].(map[string]any)
	for name, value := range obj {
		child := fmt.Sprintf("%s.%s", path, name)
		if sub, ok := props[name]; ok {
			v.apply(sub, value, child)
			continue
		}
		switch additional := s["additionalProperties"].(type) {
		case bool:
			if !additional {
				v.errorf(child, "is not an allowed property")
			}
		case map[string]any:
			v.apply(additional, value, child)
		}
	}
}

// canonical normalizes decoded values so equality and numeric checks see one
// representation: json.Number for every number and map[string]any for objects.
func canonical(v any) any {
	switch t := v.(type) {
	case json.Number:
		return t
	case int:
		return json.Number(fmt.Sprintf("%d", t))
	case int64:
		return json.Number(fmt.Sprintf("%d", t))
	case float64:
		if t == float64(int64(t)) {
			return json.Number(fmt.Sprintf("%d", int64(t)))
		}
		return json.Number(fmt.Sprintf("%g", t))
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = canonical(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = canonical(val)
		}
		return out
	}
	return v
}

func number(v any) (float64, bool) {
	switch t := canonical(v).(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func integer(v any) (int64, bool) {
	switch t := canonical(v).(type) {
	case json.Number:
		n, err := t.Int64()
		return n, err == nil
	}
	return 0, false
}

func equal(a, b any) bool { return reflect.DeepEqual(canonical(a), canonical(b)) }

func anyEqual(list []any, instance any) bool {
	for _, item := range list {
		if equal(item, instance) {
			return true
		}
	}
	return false
}

func uniqueValues(list []any) bool {
	seen := make([]string, 0, len(list))
	for _, item := range list {
		key := marshal(canonical(item))
		for _, previous := range seen {
			if previous == key {
				return false
			}
		}
		seen = append(seen, key)
	}
	return true
}

func marshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
