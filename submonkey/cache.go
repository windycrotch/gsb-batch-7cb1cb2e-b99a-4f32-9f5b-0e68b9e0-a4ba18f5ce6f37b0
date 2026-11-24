package submonkey

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vartanbeno/go-reddit/v2/reddit"
)

const (
	// runDirPrefix marks a generation-owned directory that holds its claim
	// and the private hard-linked copies of the clips being concatenated.
	runDirPrefix = ".sm-run-"
	// downloadDirPrefix marks a private staging directory for an in-flight
	// youtube-dl invocation. Nothing at the committed <id>.mp4 path exists
	// until the download has succeeded and been validated.
	downloadDirPrefix = ".sm-dl-"

	claimFileName    = "claim.json"
	runLockFileName  = "run.lock"
	dlLockFileName   = "download.lock"
	pinsDirName      = "pins"
	claimLockTimeout = 30 * time.Second
)

var (
	errLockTimeout = errors.New("timed out waiting for cache coordination lock")
	runCounter     uint64

	// cacheMu serializes coordination critical sections within this process.
	cacheMu sync.Mutex
)

// claimFile is the cross-process manifest of cache entries a generation has
// selected (or may select). Cleanup never removes a listed name while the
// owning run lock is alive.
type claimFile struct {
	RunID     string    `json:"run_id"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Files     []string  `json:"files"`
}

type cacheRun struct {
	dir       string
	keep      time.Duration
	id        string
	runDir    string
	pinsDir   string
	claimPath string
	lockPath  string
	runLock   *fileLock
	claimed   map[string]bool
}

// withCacheLock runs fn inside the cross-process critical section for dir.
// The lock file itself lives in the per-user temp directory so that a plain
// cleanCache call never has to leave coordination artifacts in the cache
// directory. If locking is unavailable, execution degrades to the
// in-process mutex rather than failing the generation.
func withCacheLock(dir string, fn func() error) error {
	cacheMu.Lock()
	defer cacheMu.Unlock()

	lk, err := acquireFileLock(globalLockPath(dir), claimLockTimeout)
	if err != nil {
		log.Printf("submonkey: cache lock unavailable (%s); continuing without cross-process lock", err)
		return fn()
	}
	defer func() { _ = lk.release() }()
	return fn()
}

func globalLockPath(cacheDir string) string {
	abs, err := filepath.Abs(cacheDir)
	if err != nil {
		abs = filepath.Clean(cacheDir)
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	sum := sha1.Sum([]byte(abs))
	key := hex.EncodeToString(sum[:8])
	return filepath.Join(os.TempDir(), "submonkey-coord", key+".lock")
}

func newCacheRun(cacheDir string, keep time.Duration) (*cacheRun, error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	id := fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), atomic.AddUint64(&runCounter, 1))
	runDir := filepath.Join(cacheDir, runDirPrefix+id)
	pinsDir := filepath.Join(runDir, pinsDirName)
	if err := os.MkdirAll(pinsDir, 0o700); err != nil {
		return nil, err
	}
	r := &cacheRun{
		dir:       cacheDir,
		keep:      keep,
		id:        id,
		runDir:    runDir,
		pinsDir:   pinsDir,
		claimPath: filepath.Join(runDir, claimFileName),
		lockPath:  filepath.Join(runDir, runLockFileName),
		claimed:   make(map[string]bool),
	}
	lk, err := acquireFileLock(r.lockPath, 5*time.Second)
	if err != nil {
		// Non-fatal: liveness based reaping degrades, but claims still protect
		// entries and private pins still protect the concat inputs.
		log.Printf("submonkey: could not acquire run lock %s: %s", r.lockPath, err)
	} else {
		r.runLock = lk
	}
	return r, nil
}

// prepare publishes the candidate claim and performs one coordinated cache
// cleanup. Candidate names are claimed before cleanup so that pre-existing
// clips this generation may select are never swept.
func (r *cacheRun) prepare(candidateNames []string) {
	_ = withCacheLock(r.dir, func() error {
		for _, name := range candidateNames {
			r.claimed[filepath.Base(name)] = true
		}
		if err := r.flushClaimLocked(); err != nil {
			log.Printf("submonkey: could not write claim: %s", err)
		}
		protected := reapCoordinationDirs(r.dir)
		for name := range r.claimed {
			protected[name] = true
		}
		cleanCacheScan(r.dir, r.keep, protected)
		return nil
	})
}

// protectAndPin atomically claims a selected cache entry and creates a private
// hard-linked copy for ffmpeg. The returned path stays valid even if another
// process removes the original cache entry.
func (r *cacheRun) protectAndPin(cachePath string) (string, error) {
	base := filepath.Base(cachePath)
	pinned := filepath.Join(r.pinsDir, base)
	var linkErr error
	err := withCacheLock(r.dir, func() error {
		if _, err := os.Stat(cachePath); err != nil {
			return err
		}
		r.claimed[base] = true
		if err := r.flushClaimLocked(); err != nil {
			log.Printf("submonkey: could not update claim: %s", err)
		}
		_ = os.Remove(pinned)
		linkErr = os.Link(cachePath, pinned)
		return nil
	})
	if err != nil {
		return "", err
	}
	if linkErr != nil {
		// Unsupported filesystem for hard links: copy instead. The claim keeps
		// the source safe while the copy runs outside the global lock.
		if err := copyFileContents(cachePath, pinned); err != nil {
			return "", err
		}
	}
	return pinned, nil
}

// finish runs the final age-based cleanup while the claim is still published,
// then removes this run's private directory and releases its liveness lock.
func (r *cacheRun) finish() {
	if r == nil {
		return
	}
	_ = withCacheLock(r.dir, func() error {
		protected := reapCoordinationDirs(r.dir)
		for name := range r.claimed {
			protected[name] = true
		}
		cleanCacheScan(r.dir, r.keep, protected)
		_ = os.Remove(r.claimPath)
		return nil
	})
	if r.runLock != nil {
		_ = r.runLock.release()
	}
	_ = os.RemoveAll(r.runDir)
	// Best-effort removal of now-empty marker directories/prefixes is not
	// needed because each run directory is uniquely named and removed whole.
}

func (r *cacheRun) flushClaimLocked() error {
	cf := claimFile{
		RunID:     r.id,
		PID:       os.Getpid(),
		StartedAt: time.Now(),
		Files:     make([]string, 0, len(r.claimed)),
	}
	for name := range r.claimed {
		cf.Files = append(cf.Files, name)
	}
	data, err := json.MarshalIndent(&cf, "", "  ")
	if err != nil {
		return err
	}
	tmp := r.claimPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, r.claimPath)
}

// cleanCache removes cache entries older than keep. It preserves the original
// mtime rule while never deleting entries protected by a live generation
// claim or coordination directories.
func cleanCache(cacheDir string, keep time.Duration) {
	if cacheDir == "" {
		return
	}
	if _, err := os.Stat(cacheDir); err != nil {
		return
	}
	_ = withCacheLock(cacheDir, func() error {
		protected := reapCoordinationDirs(cacheDir)
		cleanCacheScan(cacheDir, keep, protected)
		return nil
	})
}

// cleanCacheScan applies the historical mtime rule. The caller holds the
// cache coordination lock (or accepts same-process serialization).
func cleanCacheScan(cacheDir string, keep time.Duration, protected map[string]bool) {
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, runDirPrefix) || strings.HasPrefix(name, downloadDirPrefix) {
			continue
		} else if protected[name] {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < keep {
			continue
		}
		// os.Remove reproduces the original behavior: files and empty
		// directories are removed, non-empty directories fail silently.
		_ = os.Remove(filepath.Join(cacheDir, name))
	}
}

// reapCoordinationDirs removes run/download directories whose owner process
// has died (their OS locks are releasable) and returns the file names claimed
// by still-live runs.
func reapCoordinationDirs(cacheDir string) map[string]bool {
	protected := make(map[string]bool)
	entries, err := os.ReadDir(cacheDir)
	if err != nil {
		return protected
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() {
			continue
		}
		full := filepath.Join(cacheDir, name)
		switch {
		case strings.HasPrefix(name, runDirPrefix):
			lk, acquired, lerr := tryAcquireFileLock(filepath.Join(full, runLockFileName))
			if lerr == nil && acquired {
				// Owner is gone. A directory without a claim may simply be
				// initializing; give fresh ones a short grace period.
				if _, err := os.Stat(filepath.Join(full, claimFileName)); err == nil {
					_ = lk.release()
					_ = os.RemoveAll(full)
					continue
				}
				_ = lk.release()
				if info, err := entry.Info(); err == nil && time.Since(info.ModTime()) > time.Minute {
					_ = os.RemoveAll(full)
				}
				continue
			}
			if lk != nil {
				_ = lk.release()
			}
			for _, claimed := range readClaimNames(full) {
				protected[claimed] = true
			}
		case strings.HasPrefix(name, downloadDirPrefix):
			lk, acquired, lerr := tryAcquireFileLock(filepath.Join(full, dlLockFileName))
			if lerr == nil && acquired {
				_ = lk.release()
				_ = os.RemoveAll(full)
			} else if lk != nil {
				_ = lk.release()
			}
		}
	}
	return protected
}

func readClaimNames(runDir string) []string {
	data, err := os.ReadFile(filepath.Join(runDir, claimFileName))
	if err != nil {
		return nil
	}
	var cf claimFile
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil
	}
	names := make([]string, 0, len(cf.Files))
	for _, f := range cf.Files {
		names = append(names, filepath.Base(f))
	}
	return names
}

// candidateCacheNames returns the committed cache file names for every post
// that downloadFiles could plausibly select. Claiming a superset is safe.
func candidateCacheNames(opts *CreateOptions, posts []*reddit.Post) []string {
	names := make([]string, 0, len(posts))
	for _, post := range posts {
		if includePost(opts, post) != nil {
			continue
		}
		names = append(names, filepath.Base(committedCacheFile(opts, post)))
	}
	return names
}

func committedCacheFile(opts *CreateOptions, post *reddit.Post) string {
	return filepath.Join(opts.CacheDir, post.ID+".mp4")
}

func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
