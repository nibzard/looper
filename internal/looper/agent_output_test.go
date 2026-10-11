package looper

import (
	"encoding/json"
	"testing"
)

func TestExtractObjectWithExplanations(t *testing.T) {
	object := `{"setup":"cargo fetch --locked","verify":"cargo test","tasks":[]}`
	for _, text := range []string{object, "```json\n" + object + "\n```", "Inspection findings.\n\n```json\n" + object + "\n```\n\nNotes on commands.", "Prose\r\n```JSON\r\n" + object + "\r\n```\r\nNotes"} {
		got, err := extractObject([]byte(text))
		if err != nil || string(got) != object {
			t.Fatalf("got %s: %v", got, err)
		}
	}
}

func TestExtractObjectRejectsAmbiguity(t *testing.T) {
	for _, text := range []string{
		"```json\n{}\n```\n```json\n{}\n```",
		"```json\n{}\n```\n```sh\necho hi\n```",
		"```json\n{}", "```json\n{broken}\n```", "```json\n[]\n```", "```json\nnull\n```", "{} {}", "Prose {\"x\":1}",
	} {
		if _, err := extractObject([]byte(text)); err == nil {
			t.Fatalf("accepted ambiguous output %q", text)
		}
	}
}

// CLI argument checks must parse the schema, not just compare its string.
func TestAgentSchemasAreValidJSON(t *testing.T) {
	for name, schema := range map[string]string{"plan": planSchema, "summary": summarySchema} {
		t.Run(name, func(t *testing.T) {
			var decoded struct {
				Type       string                     `json:"type"`
				Required   []string                   `json:"required"`
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err := json.Unmarshal([]byte(schema), &decoded); err != nil {
				t.Fatalf("agent CLI rejects schema: %v", err)
			}
			if decoded.Type != "object" || len(decoded.Required) == 0 {
				t.Fatal("schema must require an object")
			}
			for _, key := range decoded.Required {
				if _, ok := decoded.Properties[key]; !ok {
					t.Fatalf("required property %s has no schema", key)
				}
			}
		})
	}
}
