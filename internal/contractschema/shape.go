package contractschema

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Shape returns a structural fingerprint of a decoded JSON document. Object
// keys map to the shape of their values, arrays list the distinct shapes of
// their items, and scalars reduce to their JSON type name. Values are
// deliberately discarded: the fingerprint records what a document contains,
// not what it says.
//
// The compatibility tests compare the shape of documents Looper produces
// against a recorded fixture. Any new, removed, renamed, or retyped field
// changes the shape and fails the test, so a contract change has to update the
// recorded fixture consciously. Formatting-only differences never appear.
func Shape(v any) any {
	switch t := canonical(v).(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = Shape(val)
		}
		return out
	case []any:
		if len(t) == 0 {
			return "empty array"
		}
		distinct := map[string]any{}
		for _, item := range t {
			s := Shape(item)
			distinct[marshal(s)] = s
		}
		keys := make([]string, 0, len(distinct))
		for key := range distinct {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out := make([]any, 0, len(keys))
		for _, key := range keys {
			out = append(out, distinct[key])
		}
		return out
	case json.Number, string, bool, nil:
		return jsonKind(t)
	}
	return fmt.Sprintf("unsupported %T", v)
}

// ShapeJSON decodes a JSON document and returns its shape.
func ShapeJSON(b []byte) (any, error) {
	var instance any
	if err := decode(b, &instance); err != nil {
		return nil, err
	}
	return Shape(instance), nil
}

// ShapeEqual reports whether two shapes are identical.
func ShapeEqual(a, b any) bool {
	return equal(Shape(a), Shape(b))
}
