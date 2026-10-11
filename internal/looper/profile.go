package looper

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// ProfileVersion is the only supported saved-settings version.
const ProfileVersion = 1

// Profile is the saved per-project preference set stored at
// runtime/profile.json. Zero values mean "unset": the empty agent follows the
// coding agent, jobs 0 stores auto mode, and empty model fields use each
// agent's own configuration. Unknown keys are preserved on save.
type Profile struct {
	Version       int                        `json:"version"`
	Agent         string                     `json:"agent"`
	ReviewAgent   string                     `json:"review_agent"`
	Jobs          int                        `json:"jobs"`
	Model         string                     `json:"model"`
	ClaudeModel   string                     `json:"claude_model"`
	Effort        string                     `json:"effort"`
	VerifyCommand string                     `json:"verify_command"`
	SetupCommand  string                     `json:"setup_command"`
	ReviewEvery   *int                       `json:"review_every,omitempty"`
	Unknown       map[string]json.RawMessage `json:"-"`
}

var profileKeys = map[string]bool{
	"version": true, "agent": true, "review_agent": true, "jobs": true,
	"model": true, "claude_model": true, "effort": true,
	"verify_command": true, "setup_command": true, "review_every": true,
}

// ProfilePath returns the saved-settings location for a project. The runtime
// directory already exists below the first projectPaths result.
func ProfilePath(c Config) string {
	_, dir := projectPaths(c)
	return filepath.Join(dir, "profile.json")
}

// LoadProfile reads and validates the saved profile. A missing file is not an
// error; it returns a nil profile. Malformed files, invalid known values, and
// unsupported versions return an error that names the path and a fix hint.
// Invalid files are never silently replaced with defaults.
func LoadProfile(path string) (*Profile, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read profile %s: %w", path, err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, profileError(path, "the file is not valid JSON (%v); fix the syntax or remove the file", err)
	}
	p := &Profile{Unknown: map[string]json.RawMessage{}}
	for key, value := range fields {
		if !profileKeys[key] {
			p.Unknown[key] = value
		}
	}
	decodeString := func(key string, into *string) error {
		v, ok := fields[key]
		if !ok {
			return nil
		}
		if err := json.Unmarshal(v, into); err != nil {
			return profileError(path, "%s must be a string; fix the value or remove the key", key)
		}
		return nil
	}
	if err := decodeString("agent", &p.Agent); err != nil {
		return nil, err
	}
	if err := decodeString("review_agent", &p.ReviewAgent); err != nil {
		return nil, err
	}
	if err := decodeString("model", &p.Model); err != nil {
		return nil, err
	}
	if err := decodeString("claude_model", &p.ClaudeModel); err != nil {
		return nil, err
	}
	if err := decodeString("effort", &p.Effort); err != nil {
		return nil, err
	}
	if err := decodeString("verify_command", &p.VerifyCommand); err != nil {
		return nil, err
	}
	if err := decodeString("setup_command", &p.SetupCommand); err != nil {
		return nil, err
	}
	if v, ok := fields["version"]; ok {
		if err := json.Unmarshal(v, &p.Version); err != nil {
			return nil, profileError(path, "version must be the number %d", ProfileVersion)
		}
	} else {
		return nil, profileError(path, "the file has no version key; add \"version\": %d or remove the file", ProfileVersion)
	}
	if v, ok := fields["jobs"]; ok {
		if err := json.Unmarshal(v, &p.Jobs); err != nil {
			return nil, profileError(path, "jobs must be a nonnegative integer; use 0 for auto mode")
		}
	}
	if v, ok := fields["review_every"]; ok {
		var n int
		if err := json.Unmarshal(v, &n); err != nil {
			return nil, profileError(path, "review_every must be a nonnegative integer")
		}
		p.ReviewEvery = &n
	}
	if err := p.Validate(); err != nil {
		return nil, profileError(path, "%v; fix the value or remove the file", err)
	}
	return p, nil
}

func profileError(path, format string, args ...any) error {
	return fmt.Errorf("profile %s: %s", path, fmt.Sprintf(format, args...))
}

// Validate rejects known keys with values this version cannot apply.
func (p *Profile) Validate() error {
	if p.Version != ProfileVersion {
		return fmt.Errorf("unsupported profile version %d; this Looper reads version %d", p.Version, ProfileVersion)
	}
	for _, agent := range []string{p.Agent, p.ReviewAgent} {
		if agent != "" && agent != "codex" && agent != "claude" {
			return fmt.Errorf("invalid agent %q; use codex or claude", agent)
		}
	}
	if p.Jobs < 0 {
		return fmt.Errorf("jobs must be a nonnegative integer; use 0 for auto mode")
	}
	if p.ReviewEvery != nil && *p.ReviewEvery < 0 {
		return fmt.Errorf("review_every must be a nonnegative integer")
	}
	return nil
}

// MarshalJSON writes known keys in a stable order and preserves unknown keys.
func (p Profile) MarshalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	reviewEvery := 5
	if p.ReviewEvery != nil {
		reviewEvery = *p.ReviewEvery
	}
	ordered := []struct {
		key   string
		value any
	}{
		{"version", p.Version}, {"agent", p.Agent}, {"review_agent", p.ReviewAgent},
		{"jobs", p.Jobs}, {"model", p.Model}, {"claude_model", p.ClaudeModel},
		{"effort", p.Effort}, {"verify_command", p.VerifyCommand},
		{"setup_command", p.SetupCommand}, {"review_every", reviewEvery},
	}
	out := []byte{'{'}
	for i, field := range ordered {
		if i > 0 {
			out = append(out, ',')
		}
		key, err := json.Marshal(field.key)
		if err != nil {
			return nil, err
		}
		value, err := json.Marshal(field.value)
		if err != nil {
			return nil, err
		}
		out = append(append(out, key...), ':')
		out = append(out, value...)
	}
	keys := make([]string, 0, len(p.Unknown))
	for key := range p.Unknown {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		k, err := json.Marshal(key)
		if err != nil {
			return nil, err
		}
		out = append(append(append(out, ','), k...), ':')
		out = append(out, p.Unknown[key]...)
	}
	return append(out, '}'), nil
}

// SaveProfile validates and atomically writes the profile with mode 0600.
func SaveProfile(path string, p *Profile) error {
	if err := p.Validate(); err != nil {
		return profileError(path, "%v", err)
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), 0600)
}

// profileDigest fingerprints the stored file bytes so settings requests can
// detect concurrent manual edits. A missing file digests its absence.
func profileDigest(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return digest(raw)
}

// mergeInto applies every set profile field as the lowest non-default layer.
func (p *Profile) mergeInto(s *settingSources) {
	if p == nil {
		return
	}
	if p.Agent != "" {
		s.agent = inputValue{p.Agent, SourceProfile}
	}
	if p.ReviewAgent != "" {
		s.reviewAgent = inputValue{p.ReviewAgent, SourceProfile}
	}
	if p.Jobs > 0 {
		s.jobs = inputValue{strconv.Itoa(p.Jobs), SourceProfile}
	} else if s.jobs.from == SourceDefault {
		s.jobs = inputValue{"auto", SourceProfile}
	}
	if p.Model != "" {
		s.model = inputValue{p.Model, SourceProfile}
	}
	if p.ClaudeModel != "" {
		s.claudeModel = inputValue{p.ClaudeModel, SourceProfile}
	}
	if p.Effort != "" {
		s.effort = inputValue{p.Effort, SourceProfile}
	}
	if p.VerifyCommand != "" {
		s.verify = inputValue{p.VerifyCommand, SourceProfile}
	}
	if p.SetupCommand != "" {
		s.setup = inputValue{p.SetupCommand, SourceProfile}
	}
	if p.ReviewEvery != nil {
		s.reviewEvery = intValue{*p.ReviewEvery, SourceProfile}
	}
}
