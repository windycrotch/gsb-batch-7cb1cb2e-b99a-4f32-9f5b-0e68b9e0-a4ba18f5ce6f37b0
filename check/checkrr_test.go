package check

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/aetaric/checkrr/logging"
	"github.com/knadh/koanf/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	log "github.com/sirupsen/logrus"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/text/language"
)

// newTestCheckrr builds a Checkrr instance backed by a temporary bolt DB and
// real locale data, with all ffprobe/ffmpeg based checks disabled so runs are
// fully local and deterministic.
func newTestCheckrr(t *testing.T, checkpaths []string) (*Checkrr, chan []string) {
	t.Helper()

	db, err := bolt.Open(filepath.Join(t.TempDir(), "checkrr-test.db"), 0600, nil)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	for _, bucket := range []string{"Checkrr", "Checkrr-files", "Checkrr-stats"} {
		err := db.Update(func(tx *bolt.Tx) error {
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
	localizer := i18n.NewLocalizer(bundle, "en")

	lastResort := log.New()
	lastResort.SetOutput(io.Discard)
	logger := &logging.Log{LastResort: lastResort, Localizer: localizer}

	conf := koanf.New(".")
	if err := conf.Set("checkpath", checkpaths); err != nil {
		t.Fatalf("set checkpath: %v", err)
	}

	ch := make(chan []string, 1)
	c := &Checkrr{
		Chan:       &ch,
		DB:         db,
		Logger:     logger,
		FullConfig: koanf.New("."),
		Localizer:  localizer,
	}
	c.FromConfig(conf)
	return c, ch
}

func waitForChannel(t *testing.T, ch chan []string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for run completion signal")
	}
}

func waitForNotRunning(t *testing.T, c *Checkrr) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for c.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("run slot was not released in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func statsRunning(t *testing.T, c *Checkrr) bool {
	t.Helper()
	running := false
	found := false
	err := c.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-stats"))
		v := b.Get([]byte("current-stats"))
		if v == nil {
			return nil
		}
		found = true
		var s map[string]interface{}
		if err := json.Unmarshal(v, &s); err != nil {
			return err
		}
		running, _ = s["running"].(bool)
		return nil
	})
	if err != nil {
		t.Fatalf("read stats: %v", err)
	}
	if !found {
		t.Fatal("no current-stats record persisted")
	}
	return running
}

// A run in progress must reject both the async (web) and synchronous (cron)
// trigger paths, and the slot must be free again once the run ends.
func TestRunRejectsConcurrentTrigger(t *testing.T) {
	c, ch := newTestCheckrr(t, []string{t.TempDir()})

	started := make(chan struct{})
	unblock := make(chan struct{})
	var once sync.Once
	c.runHook = func() {
		once.Do(func() { close(started) })
		<-unblock
	}

	if !c.RunAsync() {
		t.Fatal("first RunAsync should start a run")
	}
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not reach the walk phase")
	}

	if !c.IsRunning() {
		t.Fatal("IsRunning should be true while a run is active")
	}
	if c.RunAsync() {
		t.Fatal("second RunAsync must be rejected while a run is active")
	}

	// The cron path (Run) must also refuse and return promptly instead of
	// silently dropping the trigger or blocking forever.
	done := make(chan struct{})
	go func() {
		c.Run()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run should return immediately when a run is already active")
	}
	if !c.IsRunning() {
		t.Fatal("rejected triggers must not disturb the active run")
	}

	close(unblock)
	waitForChannel(t, ch)
	waitForNotRunning(t, c)

	// After the run finishes, a new trigger must be accepted again.
	if !c.RunAsync() {
		t.Fatal("RunAsync after completion should start a new run")
	}
	waitForChannel(t, ch)
	waitForNotRunning(t, c)
}

// A directory that fails to walk must not wedge the runner: the run slot and
// the persisted stats flag have to be released for the next trigger.
func TestRunReleasesFlagOnWalkError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	c, ch := newTestCheckrr(t, []string{missing})

	c.Run() // synchronous, like the cron scheduler

	if c.IsRunning() {
		t.Fatal("run slot must be released after a walk error")
	}
	waitForChannel(t, ch)
	if statsRunning(t, c) {
		t.Fatal("persisted stats must not say running after the run ended")
	}

	// The next trigger has to work.
	c.Run()
	if c.IsRunning() {
		t.Fatal("run slot must be released after the follow-up run")
	}
	waitForChannel(t, ch)
}

// If a run dies partway through (simulated here with a panic), the run slot
// must still be released so later triggers are not stuck on "already running".
func TestRunReleasesFlagOnPanic(t *testing.T) {
	c, ch := newTestCheckrr(t, []string{t.TempDir()})
	c.runHook = func() { panic("simulated crash mid-run") }

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected the panic to propagate to the caller")
			}
		}()
		c.Run()
	}()

	if c.IsRunning() {
		t.Fatal("run slot must be released even when a run panics")
	}
	if statsRunning(t, c) {
		t.Fatal("persisted stats must not say running after a crashed run")
	}

	// Recovery: the next trigger starts a normal run.
	c.runHook = nil
	c.Run()
	if c.IsRunning() {
		t.Fatal("run slot must be released after the recovery run")
	}
	waitForChannel(t, ch)
}

// A normal run over a directory: a plain-text file is counted as non-video,
// an unrecognized binary file lands on the bad-file list, hashes are stored,
// and all run state is cleaned up afterwards.
func TestFullRunLifecycle(t *testing.T) {
	mediaDir := t.TempDir()
	textFile := filepath.Join(mediaDir, "notes.txt")
	// Larger than the 33KB sniff buffer so content detection sees pure text.
	textData := make([]byte, 0, 65536)
	for len(textData) < 65536 {
		textData = append(textData, []byte("hello world, this is plain text\n")...)
	}
	if err := os.WriteFile(textFile, textData, 0644); err != nil {
		t.Fatal(err)
	}
	binFile := filepath.Join(mediaDir, "random.bin")
	binData := make([]byte, 512)
	for i := range binData {
		binData[i] = byte(i % 256)
	}
	if err := os.WriteFile(binFile, binData, 0644); err != nil {
		t.Fatal(err)
	}

	c, ch := newTestCheckrr(t, []string{mediaDir})
	c.Run()

	if c.IsRunning() {
		t.Fatal("run slot must be released after a completed run")
	}
	waitForChannel(t, ch)

	if c.Stats.FilesChecked != 2 {
		t.Errorf("FilesChecked = %d, want 2", c.Stats.FilesChecked)
	}
	if c.Stats.NonVideo != 1 {
		t.Errorf("NonVideo = %d, want 1", c.Stats.NonVideo)
	}
	if c.Stats.UnknownFileCount != 1 {
		t.Errorf("UnknownFileCount = %d, want 1", c.Stats.UnknownFileCount)
	}
	if statsRunning(t, c) {
		t.Error("persisted stats must not say running after the run ended")
	}

	// Only video/audio files get a hash recorded; a text file must not have
	// one (and must not be re-checked as media on the next run either).
	err := c.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr"))
		if b.Get([]byte(textFile)) != nil {
			t.Error("text files should not get a media hash")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read Checkrr bucket: %v", err)
	}

	// The unrecognized file is on the bad-file list, keyed by its path.
	err = c.DB.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte("Checkrr-files"))
		v := b.Get([]byte(binFile))
		if v == nil {
			t.Error("unrecognized file was not recorded as a bad file")
			return nil
		}
		var bad BadFile
		if err := json.Unmarshal(v, &bad); err != nil {
			return err
		}
		if bad.Service != "unknown" {
			t.Errorf("bad file service = %q, want %q", bad.Service, "unknown")
		}
		if bad.Reacquire {
			t.Error("unknown bad file should not be marked for reacquire")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read Checkrr-files bucket: %v", err)
	}
}
