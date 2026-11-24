package features

import (
	"encoding/json"
	"path/filepath"
	"testing"

	bolt "go.etcd.io/bbolt"
)

func openTestDB(t *testing.T) *bolt.DB {
	t.Helper()
	db, err := bolt.Open(filepath.Join(t.TempDir(), "stats-test.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func putCurrentStats(t *testing.T, db *bolt.DB, stats map[string]interface{}) {
	t.Helper()
	data, err := json.Marshal(stats)
	if err != nil {
		t.Fatalf("marshal stats: %v", err)
	}
	err = db.Update(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte("Checkrr-stats"))
		if err != nil {
			return err
		}
		return b.Put([]byte("current-stats"), data)
	})
	if err != nil {
		t.Fatalf("seed current-stats: %v", err)
	}
}

func getCurrentStatsRunning(t *testing.T, db *bolt.DB) (running bool, filesChecked float64) {
	t.Helper()
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-stats"))
		var s map[string]interface{}
		if err := json.Unmarshal(b.Get([]byte("current-stats")), &s); err != nil {
			return err
		}
		running, _ = s["running"].(bool)
		filesChecked, _ = s["filesChecked"].(float64)
		return nil
	})
	if err != nil {
		t.Fatalf("read current-stats: %v", err)
	}
	return running, filesChecked
}

// A process killed mid-run leaves "running": true in the persisted stats.
// ClearRunningFlag (used at startup) must reset it without touching the rest
// of the record, so the next run does not look permanently stuck.
func TestClearRunningFlag(t *testing.T) {
	db := openTestDB(t)

	// No bucket yet: nothing to clear, no error.
	wasRunning, err := ClearRunningFlag(db)
	if err != nil {
		t.Fatalf("ClearRunningFlag on empty db: %v", err)
	}
	if wasRunning {
		t.Fatal("empty db should not report a stale run")
	}

	// Simulate a crashed run.
	putCurrentStats(t, db, map[string]interface{}{"running": true, "filesChecked": 42})

	wasRunning, err = ClearRunningFlag(db)
	if err != nil {
		t.Fatalf("ClearRunningFlag: %v", err)
	}
	if !wasRunning {
		t.Fatal("expected the stale running flag to be detected")
	}

	running, filesChecked := getCurrentStatsRunning(t, db)
	if running {
		t.Fatal("running flag must be cleared")
	}
	if filesChecked != 42 {
		t.Fatalf("other stats fields must be preserved, filesChecked = %v", filesChecked)
	}

	// Idempotent: a clean record stays clean.
	wasRunning, err = ClearRunningFlag(db)
	if err != nil {
		t.Fatalf("ClearRunningFlag second call: %v", err)
	}
	if wasRunning {
		t.Fatal("second call must not report a stale run")
	}
}
