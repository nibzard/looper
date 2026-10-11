package looper

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// plannerActivity extracts observable actions, not private reasoning or tool output.
func plannerActivity(line []byte) []string {
	var event struct {
		Type    string `json:"type"`
		IsError bool   `json:"is_error"`
		Message struct {
			Content []struct {
				Type    string `json:"type"`
				Name    string `json:"name"`
				IsError bool   `json:"is_error"`
				Input   struct {
					File    string `json:"file_path"`
					Pattern string `json:"pattern"`
					Path    string `json:"path"`
					Command string `json:"command"`
				} `json:"input"`
			} `json:"content"`
		} `json:"message"`
		Item struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"item"`
	}
	if json.Unmarshal(line, &event) != nil {
		return nil
	}
	var actions []string
	for _, block := range event.Message.Content {
		if block.Type == "tool_use" {
			detail := block.Input.File
			if detail == "" {
				detail = block.Input.Pattern
			}
			if detail == "" {
				detail = block.Input.Command
			}
			if block.Input.Path != "" {
				detail += " in " + block.Input.Path
			}
			actions = append(actions, strings.TrimSpace(block.Name+" "+detail))
		} else if block.Type == "tool_result" {
			if block.IsError {
				actions = append(actions, "Tool reported an error")
			}
		}
	}
	if event.Item.Type == "command_execution" {
		prefix := "Command: "
		if event.Type == "item.completed" {
			prefix = "Command completed: "
		}
		actions = append(actions, prefix+event.Item.Command)
	}
	if event.Type == "result" {
		if event.IsError {
			actions = append(actions, "Agent reported an error")
		} else {
			actions = append(actions, "Agent returned a result")
		}
	}
	for i, a := range actions {
		a = strings.Join(strings.Fields(sanitize(a)), " ")
		runes := []rune(a)
		if len(runes) > 300 {
			a = string(runes[:300]) + "…"
		}
		actions[i] = a
	}
	return actions
}

// Read a bounded amount each tick. Incomplete lines wait for the next read.
// Oversized records are skipped without losing the next complete record.
type plannerActivityReader struct {
	decode   func([]byte) []string
	offset   int64
	pending  []byte
	skipping bool
}

func (r *plannerActivityReader) poll(path string, emit func(string)) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	if _, err = f.Seek(r.offset, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil || len(data) == 0 {
		return false
	}
	r.offset += int64(len(data))
	r.pending = append(r.pending, data...)
	for {
		end := bytes.IndexByte(r.pending, '\n')
		if end < 0 {
			break
		}
		if !r.skipping && end <= 1<<20 {
			decode := r.decode
			if decode == nil {
				decode = plannerActivity
			}
			for _, action := range decode(r.pending[:end]) {
				emit(action)
			}
		}
		r.skipping = false
		r.pending = r.pending[end+1:]
	}
	if len(r.pending) > 1<<20 {
		r.pending = nil
		r.skipping = true
	}
	return true
}
