package looper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Semantic backlog comparison (AX-018). Exact byte digests stay the identity
// of immutable artifacts and of every contract that requires bytes: import
// receipts, result commits, task Contract() hashes, and the export digest
// itself. The helpers here add one narrow decision on top of them: when
// task-file bytes differ from the accepted export only in formatting —
// whitespace, key order, or string escaping — the guards treat the content as
// unchanged instead of refusing.

// parseBacklogStrict parses exactly one backlog document and rejects
// duplicate object keys anywhere in it, so an ambiguous document is never
// silently normalized by last-key-wins. Unknown fields stay rejected through
// the Backlog and Task unmarshalers.
func parseBacklogStrict(data []byte) (Backlog, error) {
	if err := rejectDuplicateKeys(data); err != nil {
		return Backlog{}, err
	}
	var b Backlog
	if err := json.Unmarshal(data, &b); err != nil {
		return Backlog{}, err
	}
	return b, nil
}

// decodeBacklogStrict parses and validates one backlog document. Validation
// runs after the strict parse, so ambiguous input is rejected before any
// comparison can accept it as equivalent.
func decodeBacklogStrict(data []byte) (Backlog, error) {
	b, err := parseBacklogStrict(data)
	if err != nil {
		return Backlog{}, err
	}
	if err = b.Validate(); err != nil {
		return Backlog{}, err
	}
	return b, nil
}

// rejectDuplicateKeys walks one JSON document and reports any object key that
// appears twice, at any depth. Values are not inspected: the walk only
// decides whether the document has one unambiguous meaning.
func rejectDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return fmt.Errorf("object keys must be strings")
				}
				if seen[name] {
					return fmt.Errorf("duplicate object key %q", name)
				}
				seen[name] = true
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		case '[':
			for dec.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = dec.Token()
			return err
		}
		return nil
	}
	if err := walk(); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("the document holds more than one JSON value")
		}
		return err
	}
	return nil
}

// semanticFingerprint hashes the parsed backlog content in a canonical form.
// Object key order, whitespace, and string escaping never change it; any
// task, dependency, or other data difference does. Array order is data: a
// reordered task or source list is a content change.
func (b Backlog) semanticFingerprint() string {
	return hashJSON(struct {
		Project json.RawMessage
		Version int
		Sources []string
		Goal    string
		Tasks   []Task
	}{canonicalRawJSON(b.Project), b.Version, b.Sources, b.Goal, b.Tasks})
}

// canonicalRawJSON re-encodes one raw JSON value with sorted object keys, so
// formatting differences inside raw messages disappear. Input the comparison
// accepted is always parseable; an unparseable value is kept verbatim rather
// than invented away.
func canonicalRawJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	if out, err := json.Marshal(v); err == nil {
		return out
	}
	return raw
}

// backlogEdit classifies observed task-file bytes against the accepted
// export. Equivalent means the parsed, validated content is unchanged and
// only its formatting differs; Reason explains why that decision could not
// be made, and Diff carries the content differences when it could.
type backlogEdit struct {
	Observed   string // digest of the observed bytes
	Changed    bool   // the bytes differ from the accepted export digest
	Equivalent bool   // the parsed content equals the accepted content
	Semantics  string // the accepted semantic fingerprint the decision used
	Reason     string
	Diff       *backlogDifference
}

// acceptedSemantics is the semantic fingerprint of the content the accepted
// export digest hashes. A pending export already mutated the in-memory
// backlog, so only the persisted fingerprint can classify edits then; a
// legacy state without one stays undecided rather than guessing.
func (s *State) acceptedSemantics() string {
	if s == nil {
		return ""
	}
	if s.ExportSemantics != "" {
		return s.ExportSemantics
	}
	if !s.ExportPending {
		return s.Backlog.semanticFingerprint()
	}
	return ""
}

// classifyBacklog decides whether observed task-file bytes changed the
// accepted backlog content or only its formatting. It never decides from an
// invalid document: a parse or validation failure keeps its reason.
func (e *Engine) classifyBacklog(data []byte) backlogEdit {
	observed := digest(data)
	edit := backlogEdit{Observed: observed, Semantics: e.State.acceptedSemantics()}
	if observed == e.State.ExportHash {
		edit.Equivalent = true
		return edit
	}
	edit.Changed = true
	if edit.Semantics == "" {
		edit.Reason = "the accepted backlog content: the pending export saved no semantic fingerprint of it"
		return edit
	}
	parsed, err := decodeBacklogStrict(data)
	if err != nil {
		edit.Reason = "the parsed task-file comparison: " + err.Error()
		return edit
	}
	if parsed.semanticFingerprint() == edit.Semantics {
		edit.Equivalent = true
		return edit
	}
	diff := compareBacklog(e.State.Backlog, data)
	edit.Diff = &diff
	return edit
}

// editDifference renders one classification as guard evidence: the computed
// content difference, the reason no comparison could run, or an explicit
// statement that none ran. A classification that refused never presents an
// uncomputed comparison as an empty one.
func editDifference(edit backlogEdit) *backlogDifference {
	switch {
	case edit.Diff != nil:
		return edit.Diff
	case edit.Reason != "":
		return &backlogDifference{Unknown: edit.Reason}
	default:
		return &backlogDifference{Unknown: "the parsed task-file comparison did not run"}
	}
}

// classifyBacklogFile reads the task file and classifies it against the
// accepted export. An unreadable file stays a change with its reason, never
// an equivalence.
func (e *Engine) classifyBacklogFile() backlogEdit {
	data, err := os.ReadFile(e.Config.Todo)
	if err != nil {
		return backlogEdit{Changed: true, Reason: "the task file: " + err.Error()}
	}
	return e.classifyBacklog(data)
}

// adoptBacklogBytes accepts equivalent task-file bytes as the new accepted
// export without rewriting them. The operator's formatting stays on the
// file, the accepted digest moves to the observed bytes, and one event
// records both digests so recovery keeps the evidence of what was accepted
// and what replaced it. The saved backlog itself is unchanged: the content
// is the same.
func (e *Engine) adoptBacklogBytes(edit backlogEdit) error {
	if !edit.Equivalent {
		return fmt.Errorf("only equivalent task-file bytes can be adopted")
	}
	if edit.Observed == e.State.ExportHash {
		return nil // the accepted export already holds these bytes
	}
	previous := e.State.ExportHash
	e.State.ExportHash = edit.Observed
	if e.State.ExportSemantics == "" {
		e.State.ExportSemantics = edit.Semantics
	}
	// The adopted bytes are the accepted bytes now; record them before any
	// journal overwrites the file.
	e.retainAcceptedTaskFile()
	// Commit the adopted digest before the journal note, the same ordering
	// the refresh path keeps, so a reader pairing state with the cursor sees
	// the adoption event with the state that matches it.
	if err := e.save(); err != nil {
		return err
	}
	data := map[string]any{"expected_digest": previous, "observed_digest": edit.Observed}
	e.eventData("backlog-format", "The task file changed only in formatting; the equivalent content continues the saved contract.", e.identity().merge(data))
	return nil
}
