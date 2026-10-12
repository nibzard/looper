package looper

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type sessionExit struct {
	Session     string `json:"session"`
	Reason      string `json:"reason"`
	ExitCode    int    `json:"exit_code"`
	Error       string `json:"error,omitempty"`
	LastEventID int64  `json:"last_event_id"`
}

type previousSession struct {
	ID, Outcome, Reason string
	Unclean             bool
	LastEventID         int64
	ShutdownEventID     int64
}

func (s *Store) loadPreviousSession() error {
	var previous previousSession
	var ended sql.NullString
	err := s.db.QueryRow("SELECT id,outcome,ended FROM sessions ORDER BY rowid DESC LIMIT 1").Scan(&previous.ID, &previous.Outcome, &ended)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	previous.Unclean = !ended.Valid
	if err = s.db.QueryRow("SELECT coalesce(max(id),0) FROM events").Scan(&previous.LastEventID); err != nil {
		return err
	}
	// Old sessions can have an end timestamp without a recorded reason.
	previous.Reason = previous.Outcome
	rows, err := s.db.Query("SELECT id,data FROM events WHERE kind='shutdown' ORDER BY id DESC")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var raw string
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		var exit sessionExit
		if json.Unmarshal([]byte(raw), &exit) == nil && exit.Session == previous.ID {
			previous.Reason = exit.Reason
			previous.ShutdownEventID = id
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	s.previous = &previous
	return nil
}

// endSession commits the exit reason and session end in one transaction.
// A failed write leaves an open session for the next controller to detect.
func (s *Store) endSession(exit sessionExit) error {
	s.shutdownAttempted = true
	exit.Session = s.session
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := tx.QueryRow("SELECT coalesce(max(id),0) FROM events").Scan(&exit.LastEventID); err != nil {
		return err
	}
	data, err := json.Marshal(exit)
	if err != nil {
		return err
	}
	outcome := "stopped"
	if exit.Reason == "error" {
		outcome = "failed"
	}
	if exit.Reason == "cancelled" {
		outcome = "cancelled"
	}
	if exit.Reason == "panic" {
		outcome = "panicked"
	}
	ended := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.Exec("UPDATE sessions SET ended=?,outcome=? WHERE id=? AND ended IS NULL", ended, outcome, s.session); err != nil {
		return err
	}
	message := fmt.Sprintf("Controller session %s ended: %s (exit %d).", s.session, exit.Reason, exit.ExitCode)
	if exit.Error != "" {
		message += " " + exit.Error
	}
	if _, err := tx.Exec("INSERT INTO events(time,kind,message,data) VALUES(?,'shutdown',?,?)", ended, message, string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

func (e *Engine) resumeMessage() string {
	message := "Resuming retained work."
	if previous := e.Store.previous; previous != nil {
		if previous.Unclean {
			return fmt.Sprintf("%s Previous session %s ended uncleanly; its cause is unknown. Last recorded event: %d.", message, previous.ID, previous.LastEventID)
		}
		message += fmt.Sprintf(" Previous session %s ended: %s.", previous.ID, previous.Reason)
		if previous.ShutdownEventID != 0 {
			message += fmt.Sprintf(" Exit record: event %d.", previous.ShutdownEventID)
		}
	}
	return message
}

func (e *Engine) previousExitData() any {
	previous := e.Store.previous
	if previous == nil {
		return nil
	}
	reason := previous.Reason
	if previous.Unclean {
		reason = "unknown"
	}
	return map[string]any{"session": e.Store.session, "previous_session": previous.ID, "unclean": previous.Unclean, "previous_exit_reason": reason, "last_event_id": previous.LastEventID, "shutdown_event_id": previous.ShutdownEventID}
}
