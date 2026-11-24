package webserver

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aetaric/checkrr/check"
	"github.com/aetaric/checkrr/logging"
	"github.com/gin-gonic/gin"
	"github.com/knadh/koanf/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/text/language"
)

// setupTestWebserver wires the package-level dependencies the handlers use
// and returns a router that serves the API without listening on a port.
func setupTestWebserver(t *testing.T) (*gin.Engine, *check.Checkrr) {
	t.Helper()

	testDB, err := bolt.Open(filepath.Join(t.TempDir(), "webserver-test.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = testDB.Close() })

	for _, bucket := range []string{"Checkrr", "Checkrr-files", "Checkrr-stats"} {
		err := testDB.Update(func(tx *bolt.Tx) error {
			_, err := tx.CreateBucketIfNotExists([]byte(bucket))
			return err
		})
		if err != nil {
			t.Fatalf("create bucket %s: %v", bucket, err)
		}
	}

	bundle := i18n.NewBundle(language.English)
	bundle.RegisterUnmarshalFunc("toml", toml.Unmarshal)
	if _, err := bundle.LoadMessageFile(filepath.Join("..", "locale", "locale.en.toml")); err != nil {
		t.Fatalf("load locale file: %v", err)
	}
	localizer = i18n.NewLocalizer(bundle, "en")

	lastResort := log.New()
	lastResort.SetOutput(io.Discard)
	checkrrLogger = &logging.Log{LastResort: lastResort, Localizer: localizer}

	db = testDB
	scheduler = nil

	conf := koanf.New(".")
	if err := conf.Set("checkpath", []string{t.TempDir()}); err != nil {
		t.Fatalf("set checkpath: %v", err)
	}
	ch := make(chan []string, 1)
	c := &check.Checkrr{
		Chan:       &ch,
		DB:         testDB,
		Logger:     checkrrLogger,
		FullConfig: koanf.New("."),
		Localizer:  localizer,
	}
	c.FromConfig(conf)
	checkrrInstance = c

	w := &Webserver{BaseURL: BaseURL("/"), FullConfig: koanf.New(".")}
	return setupRouter(w), c
}

func seedBadFiles(t *testing.T, paths ...string) {
	t.Helper()
	err := db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-files"))
		for _, p := range paths {
			j, err := json.Marshal(check.BadFile{Service: "unknown", FileExt: filepath.Ext(p), Reason: "test"})
			if err != nil {
				return err
			}
			if err := b.Put([]byte(p), j); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed bad files: %v", err)
	}
}

func listBadFilePaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	err := db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-files"))
		return b.ForEach(func(k, v []byte) error {
			paths = append(paths, string(k))
			return nil
		})
	})
	if err != nil {
		t.Fatalf("list bad files: %v", err)
	}
	return paths
}

func postJSON(t *testing.T, router *gin.Engine, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// Triggering a run while one is active must produce an explicit conflict,
// not a silent drop; once the slot is free the endpoint must start a run.
func TestRunEndpointReportsConflict(t *testing.T) {
	router, c := setupTestWebserver(t)

	// Simulate an active run by holding the run slot.
	if !c.TryAcquire() {
		t.Fatal("could not acquire run slot")
	}

	w := postJSON(t, router, "/api/run", `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (409 Conflict)", w.Code, http.StatusConflict)
	}
	var conflictResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &conflictResp); err != nil {
		t.Fatalf("decode conflict response: %v", err)
	}
	if started, _ := conflictResp["started"].(bool); started {
		t.Error("conflict response must report started=false")
	}
	if running, _ := conflictResp["running"].(bool); !running {
		t.Error("conflict response must report running=true")
	}

	c.Release()

	w = postJSON(t, router, "/api/run", `{}`)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (202 Accepted)", w.Code, http.StatusAccepted)
	}
	var acceptResp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &acceptResp); err != nil {
		t.Fatalf("decode accept response: %v", err)
	}
	if started, _ := acceptResp["started"].(bool); !started {
		t.Error("accept response must report started=true")
	}

	// The run that was started must finish and release the slot.
	deadline := time.Now().Add(10 * time.Second)
	for c.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("run started via the API did not finish in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Deletion is by stable path key: unknown paths are ignored and reported,
// existing ones are removed, and nothing else is touched.
func TestDeleteBadFilesByPath(t *testing.T) {
	router, _ := setupTestWebserver(t)
	seedBadFiles(t, "/media/a.mkv", "/media/b.mkv", "/media/c.mkv")

	w := postJSON(t, router, "/api/files/bad", `["/media/a.mkv","/media/missing.mkv"]`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Deleted  []string `json:"deleted"`
		NotFound []string `json:"notFound"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Deleted) != 1 || resp.Deleted[0] != "/media/a.mkv" {
		t.Errorf("deleted = %v, want [/media/a.mkv]", resp.Deleted)
	}
	if len(resp.NotFound) != 1 || resp.NotFound[0] != "/media/missing.mkv" {
		t.Errorf("notFound = %v, want [/media/missing.mkv]", resp.NotFound)
	}

	remaining := listBadFilePaths(t)
	if len(remaining) != 2 {
		t.Fatalf("remaining records = %v, want 2", remaining)
	}
	for _, p := range remaining {
		if p == "/media/a.mkv" {
			t.Error("/media/a.mkv should have been deleted")
		}
	}

	// The list endpoint and the stats bad-file count must agree.
	req := httptest.NewRequest(http.MethodGet, "/api/files/bad", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var files []badFileData
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatalf("decode files list: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("files list length = %d, want 2", len(files))
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stats/current", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var stats Stats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.BadFiles != uint64(len(files)) {
		t.Errorf("badFiles = %d, but the list has %d records", stats.BadFiles, len(files))
	}
}

// Deleting a path that was never recorded (or was already deleted) must not
// remove or corrupt any other record.
func TestDeleteBadFilesIgnoresUnknownPaths(t *testing.T) {
	router, _ := setupTestWebserver(t)
	seedBadFiles(t, "/media/a.mkv", "/media/b.mkv")

	w := postJSON(t, router, "/api/files/bad", `["/media/never-existed.mkv"]`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var resp struct {
		Deleted  []string `json:"deleted"`
		NotFound []string `json:"notFound"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Deleted) != 0 {
		t.Errorf("deleted = %v, want empty", resp.Deleted)
	}
	if len(resp.NotFound) != 1 {
		t.Errorf("notFound = %v, want 1 entry", resp.NotFound)
	}
	if remaining := listBadFilePaths(t); len(remaining) != 2 {
		t.Fatalf("remaining records = %v, want both records untouched", remaining)
	}
}

// Cleanup (deletes) and an active run (writes) can happen at the same time.
// The final list must contain exactly the records that exist in the DB, and
// the stats count must match it.
func TestConcurrentDeleteAndRecord(t *testing.T) {
	router, _ := setupTestWebserver(t)

	const seeded = 50
	const added = 50
	seedPaths := make([]string, 0, seeded)
	for i := 0; i < seeded; i++ {
		seedPaths = append(seedPaths, fmt.Sprintf("/media/seed-%d.mkv", i))
	}
	seedBadFiles(t, seedPaths...)

	var wg sync.WaitGroup

	// Deleters: remove the seeded records in batches, like a user cleaning
	// the list from the web UI.
	batch := seeded / 5
	for i := 0; i < 5; i++ {
		paths := seedPaths[i*batch : (i+1)*batch]
		wg.Add(1)
		go func(paths []string) {
			defer wg.Done()
			deleted, notFound, err := deleteBadFilesByPath(paths)
			if err != nil {
				t.Errorf("deleteBadFilesByPath: %v", err)
			}
			if len(notFound) != 0 {
				t.Errorf("unexpected notFound: %v", notFound)
			}
			if len(deleted) != len(paths) {
				t.Errorf("deleted %d of %d paths", len(deleted), len(paths))
			}
		}(paths)
	}

	// Writer: an active run keeps recording new bad files while the cleanup
	// above is in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < added; i++ {
			path := fmt.Sprintf("/media/new-%d.mkv", i)
			err := db.Update(func(tx *bolt.Tx) error {
				b := tx.Bucket([]byte("Checkrr-files"))
				j, _ := json.Marshal(check.BadFile{Service: "sonarr", Reacquire: true, Reason: "concurrent"})
				return b.Put([]byte(path), j)
			})
			if err != nil {
				t.Errorf("record bad file: %v", err)
			}
		}
	}()

	wg.Wait()

	// Every seeded record is gone, every concurrently written record survives.
	remaining := listBadFilePaths(t)
	if len(remaining) != added {
		t.Fatalf("remaining records = %d, want %d (%v)", len(remaining), added, remaining)
	}
	for _, p := range remaining {
		if !strings.HasPrefix(p, "/media/new-") {
			t.Errorf("unexpected record survived: %s", p)
		}
	}

	// The API list and the stats count must reflect exactly what is in the DB.
	req := httptest.NewRequest(http.MethodGet, "/api/files/bad", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var files []badFileData
	if err := json.Unmarshal(w.Body.Bytes(), &files); err != nil {
		t.Fatalf("decode files list: %v", err)
	}
	if len(files) != len(remaining) {
		t.Errorf("API list has %d records, DB has %d", len(files), len(remaining))
	}

	req = httptest.NewRequest(http.MethodGet, "/api/stats/current", nil)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var stats Stats
	if err := json.Unmarshal(w.Body.Bytes(), &stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.BadFiles != uint64(len(remaining)) {
		t.Errorf("badFiles = %d, but the DB has %d records", stats.BadFiles, len(remaining))
	}
}
