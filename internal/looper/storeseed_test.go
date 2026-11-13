package looper

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// The fixtures are what make this suite fsync-bound: every Store.Event and
// Store.Check call commits on its own, and the heavy fixtures seed hundreds
// or thousands of rows. The production calls stay exactly as they are;
// fixture seeding goes through one transaction here, so seeding two
// thousand check rows costs one durable commit instead of two thousand.
// Rows land in the same order with the same values, which is everything
// the read paths under test observe.

// seedEvents appends count events in one transaction. build returns the
// event each index produces, exactly as a loop of Store.Event calls would.
func seedEvents(t *testing.T, s *Store, count int, build func(i int) (kind, message string, data any)) {
	t.Helper()
	inOneTransaction(t, s, func(tx *sql.Tx) error {
		for i := 0; i < count; i++ {
			kind, message, data := build(i)
			b, err := json.Marshal(data)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("INSERT INTO events(time,kind,message,data) VALUES(?,?,?,?)",
				wallClock().UTC().Format(time.RFC3339Nano), kind, message, string(b)); err != nil {
				return err
			}
		}
		return nil
	})
}

// seedChecks mirrors Store.Check for fixture rows, in one transaction. Each
// row is attempt, revision, kind, result, artifact.
func seedChecks(t *testing.T, s *Store, rows [][5]string) {
	t.Helper()
	inOneTransaction(t, s, func(tx *sql.Tx) error {
		stmt, err := tx.Prepare("INSERT INTO checks(time,attempt_id,revision,kind,result,artifact) VALUES(?,?,?,?,?,?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, r := range rows {
			if _, err = stmt.Exec(wallClock().UTC().Format(time.RFC3339Nano), r[0], r[1], r[2], r[3], r[4]); err != nil {
				return err
			}
		}
		return nil
	})
}

func inOneTransaction(t *testing.T, s *Store, seed func(tx *sql.Tx) error) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = seed(tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// writeFixtureFile stores static fixture content without the two fsyncs
// atomicWrite spends per file. Fixture logs and artifacts only need to
// exist with their bytes; nothing under test observes their durability.
func writeFixtureFile(path string, data []byte) error {
	return os.WriteFile(path, data, 0600)
}
