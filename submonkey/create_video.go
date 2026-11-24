package submonkey

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"submonkey/util"
	"time"

	"github.com/vartanbeno/go-reddit/v2/reddit"
)

var (
	regexExcludeExts = regexp.MustCompile(`(png|jpe?g)$`)
)

type CreateOptions struct {
	Filter     string
	Sort       string
	Time       string
	Limit      int
	NSFW       bool
	Duration   time.Duration
	OutputSize string
	OutputFile string
	CacheDir   string
	CacheKeep  time.Duration
	Version    string
}

func CreateVideo(opts *CreateOptions) error {
	if opts == nil {
		return errors.New("missing create options")
	}
	// Normalize once so claims, coordination locks and pinned paths all refer
	// to the same cache directory regardless of the caller's working dir.
	if abs, err := filepath.Abs(opts.CacheDir); err == nil {
		opts.CacheDir = abs
	}
	if err := checkDependencies(); err != nil {
		return err
	}

	log.Printf("Retrieving %s posts for subreddit(s) %s ...", opts.Sort, opts.Filter)
	posts, err := retrievePosts(opts)
	if err != nil {
		return err
	}

	// One generation owns the lifecycle of its selected cache entries. The
	// run directory holds the claim and private pinned copies.
	run, err := newCacheRun(opts.CacheDir, opts.CacheKeep)
	if err != nil {
		return err
	}
	defer run.finish()

	// Claim every clip this run might select *before* cleaning, so cleanup
	// can never sweep a pre-existing clip that is about to be reused.
	run.prepare(candidateCacheNames(opts, posts))

	log.Printf("Downloading up to %d video(s) ...", opts.Limit)
	filenames, selectedPosts, err := downloadFiles(opts, posts)
	if err != nil {
		return err
	}

	actual := len(filenames)
	if actual == 0 {
		// Never invoke ffmpeg with zero inputs and never touch an existing
		// output file: report a clear failure instead of producing an empty,
		// seemingly successful video.
		log.Printf("No usable videos were downloaded (requested limit: %d).", opts.Limit)
		return fmt.Errorf("generation aborted: no usable video clips available for %s", opts.OutputFile)
	}
	if actual < opts.Limit {
		log.Printf("WARNING: only %d of %d requested video(s) available; continuing with the %d successfully prepared clip(s)", actual, opts.Limit, actual)
	} else {
		log.Printf("Successfully prepared %d/%d requested video(s).", actual, opts.Limit)
	}

	// Pin the selected clips into the private run directory. From here on,
	// concurrent or deferred cache cleanup cannot remove the bytes ffmpeg
	// reads, even if another process sweeps the shared cache entries.
	inputs := make([]string, 0, actual)
	usedPosts := make([]*reddit.Post, 0, actual)
	for i, filename := range filenames {
		pinned, perr := run.protectAndPin(filename)
		if perr != nil {
			// The committed clip vanished despite the claim (e.g. removed by
			// an external actor). Re-download it once rather than failing the
			// whole generation or handing ffmpeg a missing path.
			log.Printf("Cached clip %s is no longer available (%s); downloading again ...", filename, perr)
			if i >= len(selectedPosts) {
				continue
			}
			redownloaded, rerr := downloadFile(opts, selectedPosts[i], i+1)
			if rerr != nil {
				log.Printf("WARNING: skipping clip %s after re-download failed: %s", selectedPosts[i].ID, rerr)
				continue
			}
			pinned, perr = run.protectAndPin(redownloaded)
			if perr != nil {
				log.Printf("WARNING: skipping clip %s: %s", selectedPosts[i].ID, perr)
				continue
			}
			filename = redownloaded
		}
		inputs = append(inputs, pinned)
		if i < len(selectedPosts) {
			usedPosts = append(usedPosts, selectedPosts[i])
		}
	}

	if len(inputs) == 0 {
		return fmt.Errorf("generation aborted: none of the %d prepared clips could be pinned for concatenation", actual)
	}
	if len(inputs) < actual {
		log.Printf("WARNING: %d clip(s) remain available for concatenation after pinning (requested limit: %d)", len(inputs), opts.Limit)
	}

	log.Printf("Generating video %s from %d clip(s) ...", opts.OutputFile, len(inputs))
	comment := generateComment(usedPosts, opts)
	output, err := util.ConcatVideos(inputs, opts.OutputSize, comment, opts.OutputFile)
	if err != nil {
		// Do not log.Fatal here: it would exit immediately, skip the deferred
		// cache lifecycle cleanup and terminate in-process test runs.
		if len(output) > 0 {
			log.Printf("ffmpeg output:\n%s", string(output))
		}
		return fmt.Errorf("video concatenation failed: %w", err)
	}
	log.Printf("Done.")
	return nil
}

func checkDependencies() error {
	if err := util.Run("ffmpeg", "-version"); err != nil {
		return fmt.Errorf("ffmpeg check failed, please install ffmpeg: %s", err.Error())
	} else if err := util.Run("ffprobe", "-version"); err != nil {
		return fmt.Errorf("ffprobe check failed, please install ffmpeg: %s", err.Error())
	} else if err := util.Run("youtube-dl", "--version"); err != nil {
		return fmt.Errorf("youtube-dl check failed, please install youtube-dl: %s", err.Error())
	}
	return nil
}

func retrievePosts(opts *CreateOptions) (posts []*reddit.Post, err error) {
	// Test seam for end-to-end runs without access to the Reddit API. Set
	// SUBMONKEY_POSTS_FILE to a JSON array of posts (or {"posts": [...]})
	// with at least id/url fields.
	if fixture := os.Getenv("SUBMONKEY_POSTS_FILE"); fixture != "" {
		return loadPostsFixture(fixture)
	}
	ctx := context.Background()
	client := reddit.DefaultClient()
	listOpts := &reddit.ListOptions{
		Limit: 100, // always 100, so we can filter out without extra logic; if this is ever > 100, use resp.After
	}
	listPostOpts := &reddit.ListPostOptions{
		ListOptions: *listOpts,
		Time:        opts.Time,
	}
	switch opts.Sort {
	case "hot":
		posts, _, err = client.Subreddit.HotPosts(ctx, opts.Filter, listOpts)
	case "top":
		posts, _, err = client.Subreddit.TopPosts(ctx, opts.Filter, listPostOpts)
	case "rising":
		posts, _, err = client.Subreddit.RisingPosts(ctx, opts.Filter, listOpts)
	case "new":
		posts, _, err = client.Subreddit.NewPosts(ctx, opts.Filter, listOpts)
	case "controversial":
		posts, _, err = client.Subreddit.ControversialPosts(ctx, opts.Filter, listPostOpts)
	default:
		err = fmt.Errorf("invalid sort options: %s", opts.Sort)
	}
	return
}

type postsFixture struct {
	Posts []*reddit.Post `json:"posts"`
}

func loadPostsFixture(path string) ([]*reddit.Post, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read posts fixture: %w", err)
	}
	posts := make([]*reddit.Post, 0)
	if err := json.Unmarshal(data, &posts); err == nil {
		return posts, nil
	}
	var wrapped postsFixture
	if err := json.Unmarshal(data, &wrapped); err != nil {
		return nil, fmt.Errorf("parse posts fixture %s: %w", path, err)
	}
	return wrapped.Posts, nil
}

func downloadFiles(opts *CreateOptions, inposts []*reddit.Post) (filenames []string, posts []*reddit.Post, err error) {
	if err := os.MkdirAll(opts.CacheDir, 0o700); err != nil {
		return nil, nil, err
	}
	filenames = make([]string, 0)
	posts = make([]*reddit.Post, 0)
	for _, post := range inposts {
		if err := includePost(opts, post); err != nil {
			continue
		}
		filename, err := downloadFile(opts, post, len(filenames)+1)
		if err != nil {
			log.Printf("- Skipping %s: %s", post.ID, err)
			continue
		}
		filenames = append(filenames, filename)
		posts = append(posts, post)
		if len(filenames) == opts.Limit {
			break
		}
	}
	if len(filenames) < opts.Limit {
		log.Printf("- No other matching videos in listing")
	}
	return filenames, posts, nil
}

func downloadFile(opts *CreateOptions, post *reddit.Post, num int) (string, error) {
	if post == nil {
		return "", errors.New("cannot download nil post")
	}
	if err := os.MkdirAll(opts.CacheDir, 0o700); err != nil {
		return "", err
	}
	videoFile := committedCacheFile(opts, post)

	// Reuse only a committed file that exists, is non-empty and still probes
	// as valid media within the duration limit. A truncated/half file left by
	// an interrupted older run fails this check and is re-downloaded instead
	// of reused. The explicit stat gate preserves the historical existence
	// check independently of how ffprobe behaves.
	if info, statErr := os.Stat(videoFile); statErr == nil && !info.IsDir() && info.Size() > 0 {
		if meta, err := util.VideoMetadata(videoFile); err == nil {
			if opts.Duration <= 0 || meta.Duration <= opts.Duration {
				log.Printf("- Already downloaded %s (%d/%d), %s", post.ID, num, opts.Limit, post.URL)
				return videoFile, nil
			}
			log.Printf("- Cached clip %s exceeds max duration (%s > %s); replacing it", post.ID, meta.Duration, opts.Duration)
		} else {
			log.Printf("- Cached clip %s is unreadable (%s); replacing it", post.ID, err)
		}
	}
	// Remove invalid residue at the committed name and youtube-dl sidecars.
	removeCommittedResidue(videoFile)

	// Download into a private staging directory. The committed path only
	// appears after youtube-dl succeeded and the clip validated, so neither
	// cleanup nor another run can mistake an in-flight/half file for a
	// finished asset.
	stagingDir, err := os.MkdirTemp(opts.CacheDir, downloadDirPrefix+"*")
	if err != nil {
		return "", err
	}
	lk, lerr := acquireFileLock(filepath.Join(stagingDir, dlLockFileName), 5*time.Second)
	if lerr != nil {
		log.Printf("submonkey: could not lock staging dir %s: %s", stagingDir, lerr)
		lk = nil
	}
	cleanupStaging := func() {
		if lk != nil {
			_ = lk.release()
		}
		_ = os.RemoveAll(stagingDir)
	}

	stagedOutput := filepath.Join(stagingDir, post.ID+".mp4")
	args := []string{
		"--output", stagedOutput,
		"--match-filter", fmt.Sprintf("duration <? %d", int(opts.Duration.Seconds())), // "<?" ignores duration if not known
		"--merge-output-format", "mp4",
		post.URL,
	}
	cmd := exec.Command("youtube-dl", args...)
	if err := cmd.Run(); err != nil {
		cleanupStaging() // removes any half-downloaded fragments
		return "", err
	}

	produced := resolveProducedFile(stagedOutput, stagingDir, post.ID)
	meta, err := util.VideoMetadata(produced)
	if err != nil {
		cleanupStaging()
		return "", err
	}
	if meta.Duration > opts.Duration {
		cleanupStaging() // over-limit clip must not poison future runs
		return "", errors.New("video exceeds max duration")
	}

	if err := commitDownload(produced, videoFile); err != nil {
		cleanupStaging()
		return "", err
	}
	cleanupStaging()

	log.Printf("- Downloaded %s (%d/%d), %s", post.ID, num, opts.Limit, post.URL)
	return videoFile, nil
}

// resolveProducedFile returns the exact --output path when present, otherwise
// looks for the media file youtube-dl actually merged/remuxed in staging.
func resolveProducedFile(stagedOutput, stagingDir, postID string) string {
	if _, err := os.Stat(stagedOutput); err == nil {
		return stagedOutput
	}
	preferred := []string{".mp4", ".mkv", ".webm", ".mov", ".m4v"}
	for _, ext := range preferred {
		candidate := filepath.Join(stagingDir, postID+ext)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	entries, err := os.ReadDir(stagingDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			switch filepath.Ext(entry.Name()) {
			case ".part", ".ytdl", ".json", ".jpg", ".png", ".webp", ".description":
				continue
			}
			return filepath.Join(stagingDir, entry.Name())
		}
	}
	return stagedOutput
}

// commitDownload moves the validated staged clip onto the committed cache
// path. Both paths live under CacheDir, so this is normally an atomic rename.
func commitDownload(produced, videoFile string) error {
	if err := os.MkdirAll(filepath.Dir(videoFile), 0o700); err != nil {
		return err
	}
	if err := renameWithRetry(produced, videoFile); err == nil {
		return nil
	}
	// Fallback for odd cross-device/Windows replacement situations.
	if err := copyFileContents(produced, videoFile); err != nil {
		return err
	}
	return os.RemoveAll(produced)
}

// renameWithRetry performs an atomic replace, retrying briefly on Windows
// where antivirus scanning or a just-closed reader can transiently block it.
func renameWithRetry(src, dst string) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		if err = os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(time.Duration(25*(attempt+1)) * time.Millisecond)
	}
	return err
}

// removeCommittedResidue deletes a bad committed file and the fragment/
// sidecar names youtube-dl historically creates next to it.
func removeCommittedResidue(videoFile string) {
	_ = os.Remove(videoFile)
	_ = os.Remove(videoFile + ".part")
	dir := filepath.Dir(videoFile)
	id := filepath.Base(videoFile)
	for _, pattern := range []string{id + ".*", strings.TrimSuffix(id, ".mp4") + ".*"} {
		matches, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			continue
		}
		for _, match := range matches {
			_ = os.RemoveAll(match)
		}
	}
}

func includePost(opts *CreateOptions, post *reddit.Post) error {
	if post == nil {
		return errors.New("empty post")
	} else if post.URL == "" {
		return errors.New("empty URL")
	} else if post.NSFW && !opts.NSFW {
		return errors.New("tagged NSFW")
	} else if regexExcludeExts.MatchString(post.URL) {
		return errors.New("unsupported file")
	}
	return nil
}

func generateComment(posts []*reddit.Post, opts *CreateOptions) string {
	comment := fmt.Sprintf("Created with submonkey %s, https://heckel.io/submonkey\n", opts.Version)
	comment += fmt.Sprintf("Subreddit(s): %s, sorted by %s (%s)\n\n", opts.Filter, opts.Sort, opts.Time)
	comment += "Videos:\n\n"
	for i, post := range posts {
		comment += fmt.Sprintf("#%d: %s\nhttps://reddit.com%s\n%s\n\n", i+1, post.Title, post.Permalink, post.URL)
	}
	return comment
}
