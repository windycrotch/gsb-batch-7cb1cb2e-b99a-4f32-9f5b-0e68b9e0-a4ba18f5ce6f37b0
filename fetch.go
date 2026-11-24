// Package subtitles finds a subtitle for a video, checks that it actually
// belongs to that video, and fixes the timing.
//
// Subtitle sites list a lot of files per title, cut for different releases,
// and the episode tagging is not reliable. Metadata can't tell you which file
// is right. The audio can: a subtitle that belongs to a video has cue starts
// that line up with where speech begins. So every candidate is checked against
// the video before it's accepted, and the offset correction comes out of the
// same pass.
//
// The searching itself is github.com/m314dev/subapi.
//
//	speech, _ := subtitles.Listen("/media/Breaking.Bad.S01E01.mkv")
//	c := subtitles.Client{Sources: []subapi.Source{
//	    &subapi.SubDL{APIKey: subdlKey},
//	    &subapi.OpenSubtitles{APIKey: osKey, Username: u, Password: p},
//	}, Speech: speech}
//	res, err := c.Fetch(ctx, subapi.Query{TMDBID: 1396, Season: 1, Episode: 1, Lang: "ar"})
package subtitles

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/m314dev/m314ss/srt"
	"github.com/m314dev/subapi"
)

// ErrNoMatch means every candidate was either unavailable or failed to align.
var ErrNoMatch = errors.New("no subtitle matched this video")

// errTimingMismatch means a paid translation came back timed like some other
// file. It is not a lookup failure: the original subtitle was verified and is
// still usable, so translateFallback returns that instead of dropping both.
var errTimingMismatch = errors.New("translation isn't timed like the original file")

// Translator is the half of subapi.SubDL the translation fallback uses.
// *subapi.SubDL satisfies it.
type Translator interface {
	// TranslateEp queues a translation of subtitle nID and returns the job.
	TranslateEp(ctx context.Context, nID, targetLang, tone string, season, episode int) (subapi.Job, error)

	// WaitTranslation blocks until the job is downloadable.
	WaitTranslation(ctx context.Context, id string, every time.Duration) (subapi.Job, error)

	// DownloadTranslation fetches the finished subtitle.
	DownloadTranslation(ctx context.Context, job subapi.Job) ([]byte, error)
}

// Client searches sources in order and returns the first subtitle the audio
// agrees with.
type Client struct {
	Sources []subapi.Source

	// How many candidates to download from one source before moving on.
	// Downloads are the metered thing on opensubtitles. 0 means 3.
	MaxTriesPerSource int

	// What candidates get verified against. Build it with Listen. nil skips
	// verification and just returns the first thing that downloads.
	//
	// It's only onsets and window bounds, no audio, so it's small enough to
	// send over the wire. See Handler.
	Speech *Speech

	// If set, a title with no subtitle in the requested language gets searched
	// in other languages, verified the same way, and machine translated into
	// the one asked for. Costs a credit and a wait, so it's opt in. Needs a
	// SubDL pro key.
	//
	// It's an interface so tests (and anything with its own queue) can
	// substitute a fake; *subapi.SubDL implements it.
	TranslateWith Translator

	// Languages to try for the translation fallback. Empty means english
	// first, then anything.
	FallbackLangs []string

	// Translation tone, see the subapi Tone constants. Empty lets subdl pick.
	Tone string

	// Called with one line per candidate tried, if set.
	Log func(string)
}

// Result is an accepted subtitle.
type Result struct {
	Data    []byte // SRT bytes, already shifted
	Source  string
	Release string
	Sync    SyncResult

	// The source's own id for this subtitle, if it has one.
	ID string

	// Language of Data. Same as the query unless Translated.
	Lang string

	// Set when nothing existed in the requested language and this one was
	// machine translated from FromLang.
	Translated bool
	FromLang   string
}

// Fetch returns the first subtitle the video's audio agrees with.
//
// Sources are tried in order and candidates in the order the source returned
// them, so put the cheapest source first. If nothing matches the error wraps
// ErrNoMatch. That is a normal outcome, not a failure: ship without a
// subtitle rather than attach a wrong one.
//
// If ctx is canceled or times out the error is ctx.Err() (or wraps it), never
// ErrNoMatch: giving up is not the same as finding nothing, and a caller that
// stops here can retry.
func (c *Client) Fetch(ctx context.Context, q subapi.Query) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	res, err := c.search(ctx, q, nil)
	if err == nil {
		return res, nil
	}
	if !errors.Is(err, ErrNoMatch) {
		return Result{}, err
	}
	// A cancel that lands after search's last check must not be reported as
	// "no match": the caller needs to tell "gave up" from "nothing fits".
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, ctxErr
	}
	if c.TranslateWith == nil || q.Lang == "" {
		return Result{}, err
	}
	return c.translateFallback(ctx, q, err)
}

// translateFallback handles a title with no subtitle in the language we want.
//
// A subtitle in some other language is still the right subtitle for this
// video, and the audio proves it just as well since the language has nothing
// to do with when the lines are spoken. So verify one the normal way, then pay
// to translate a file we already know fits. Never translate unverified, a
// wrong subtitle in the right language is still wrong.
//
// Transcribing when there is no subtitle in any language would be the obvious
// next step, but subdl's endpoint wants a media_url its own servers can fetch
// and the video never leaves the caller's machine. See subapi.SubDL.Transcribe
// if you do have a public URL.
func (c *Client) translateFallback(ctx context.Context, q subapi.Query, prev error) (Result, error) {
	langs := c.FallbackLangs
	if len(langs) == 0 {
		langs = []string{"EN", ""}
	}

	// Candidates already downloaded and verified but unusable for
	// translation because they have no id. A search in another language
	// lists the same files again; downloading them again pays the metered
	// half for an answer we already know. Scoped to this call so concurrent
	// requests share nothing.
	skip := map[string]bool{}

	errs := []error{prev}
	for _, lang := range langs {
		if strings.EqualFold(lang, q.Lang) {
			continue // already tried
		}
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}

		alt := q
		alt.Lang = lang
		c.logf("no %s subtitle; trying %s", q.Lang, langName(lang))
		res, err := c.search(ctx, alt, skip)
		if err != nil {
			// A canceled or timed-out search is the caller giving up, not
			// another "nothing here". Stop instead of trying more languages.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			errs = append(errs, err)
			continue
		}
		if res.ID == "" {
			// translation works by id, the bytes are no use to it.
			// Remember it so a search in another language, which will list
			// the same files again, doesn't pay to download it a second time.
			c.logf("%s: %s: verified but has no id to translate", res.Source, res.Release)
			skip[candidateKey(res.Source, res.Release)] = true
			errs = append(errs, fmt.Errorf("%s: %s: verified but has no id to translate", res.Source, res.Release))
			continue
		}

		out, err := c.translate(ctx, res, q.Lang, q)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			if errors.Is(err, errTimingMismatch) {
				// The translation is of some other file, but res is still a
				// verified subtitle for this video. Return it rather than
				// failing the request and throwing the paid work away.
				c.logf("%s: %v; returning the verified %s subtitle instead",
					res.Release, err, langName(res.Lang))
				return res, nil
			}
			c.logf("translating %s: %v", res.Release, err)
			errs = append(errs, err)
			continue
		}
		return out, nil
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	return Result{}, fmt.Errorf("%w (%v)", ErrNoMatch, errors.Join(errs...))
}

// candidateKey identifies a candidate across searches in different languages.
// Candidates that can't be translated have no id, so source and release name
// are what is left to match on.
func candidateKey(source, release string) string { return source + "\x00" + release }

func (c *Client) translate(ctx context.Context, res Result, lang string, q subapi.Query) (Result, error) {
	s := c.TranslateWith
	job, err := await(ctx, func(ctx context.Context) (subapi.Job, error) {
		return s.TranslateEp(ctx, res.ID, strings.ToUpper(lang), c.Tone, q.Season, q.Episode)
	})
	if err != nil {
		return Result{}, err
	}
	c.logf("translating %s %s to %s (job %s)", langName(res.Lang), res.Release, strings.ToUpper(lang), job.Ref())

	done, err := await(ctx, func(ctx context.Context) (subapi.Job, error) {
		return s.WaitTranslation(ctx, job.Ref(), 0)
	})
	if err != nil {
		return Result{}, err
	}
	data, err := await(ctx, func(ctx context.Context) ([]byte, error) {
		return s.DownloadTranslation(ctx, done)
	})
	if err != nil {
		return Result{}, err
	}

	out := res
	out.Data = data
	out.Translated = true
	out.FromLang = res.Lang
	out.Lang = lang

	// The translation is made from the original file, not the shifted one, so
	// it needs the same correction. Re-measuring is cheap and catches a
	// translator that retimed things itself.
	if c.Speech != nil {
		if m, fixed, err := c.Speech.Match(data); err == nil && m.Matched() {
			out.Data, out.Sync = fixed, m
			return out, nil
		}
	}
	// A translated file can be thinner to align against, so it may not pass
	// on its own. The original's correction still holds if its cues are the
	// original's. If they aren't, it is a translation of some other file (a
	// season pack's id can stand for every episode in it), and shipping it
	// would put a wrong subtitle in the right language on the video.
	if res.Sync.Shifted {
		data = res.Sync.Retime(data)
	}
	if !sameTiming(data, res.Data) {
		return Result{}, fmt.Errorf("%w: %s", errTimingMismatch, res.Release)
	}
	out.Data = data
	return out, nil
}

// sameTiming reports whether most cues of a start where a cue of b does: a
// translation keeps the original's timestamps, merging or splitting a few
// lines at most. Another file's cues land near b's only by chance.
func sameTiming(a, b []byte) bool {
	as, bs := srt.Starts(srt.Parse(a)), srt.Starts(srt.Parse(b))
	if len(as) == 0 || len(bs) == 0 {
		return false
	}
	sort.Float64s(bs)
	near := 0
	for _, t := range as {
		i := sort.SearchFloat64s(bs, t-0.1)
		if i < len(bs) && bs[i] <= t+0.1 {
			near++
		}
	}
	return float64(near) >= 0.8*float64(len(as))
}

func (c *Client) logf(format string, args ...any) {
	if c.Log != nil {
		c.Log(fmt.Sprintf(format, args...))
	}
}

func langName(lang string) string {
	if lang == "" {
		return "any language"
	}
	return strings.ToUpper(lang)
}

// await runs f and stops waiting on it as soon as ctx is done.
//
// The sources and the translator take ctx and pass it to their HTTP calls, so
// usually this is just f's own return. But one of them ignoring ctx must not
// pin a canceled request to a download or a translation poll that nobody is
// waiting for anymore. The goroutine is left to finish on its own; its result
// is dropped.
func await[T any](ctx context.Context, f func(context.Context) (T, error)) (T, error) {
	if err := ctx.Err(); err != nil {
		var zero T
		return zero, err
	}
	type outcome struct {
		v   T
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		v, err := f(ctx)
		ch <- outcome{v, err}
	}()
	// Prefer a result that is already there: work that finished before the
	// cancel shouldn't be thrown away by a coin flip.
	select {
	case o := <-ch:
		return o.v, o.err
	default:
	}
	select {
	case o := <-ch:
		return o.v, o.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// search tries the sources in order and returns the first verified candidate.
//
// skip holds candidates that a previous search already downloaded and found
// unusable; they are not downloaded again. It may be nil.
func (c *Client) search(ctx context.Context, q subapi.Query, skip map[string]bool) (Result, error) {
	tries := c.MaxTriesPerSource
	if tries <= 0 {
		tries = 3
	}
	logf := c.logf

	var errs []error
	for _, src := range c.Sources {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		candidates, err := await(ctx, func(ctx context.Context) ([]subapi.Candidate, error) {
			return src.Find(ctx, q)
		})
		if err != nil {
			// A canceled search is the caller giving up, not that source
			// being empty. Moving to the next source would keep the request
			// alive after the caller is gone.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Result{}, ctxErr
			}
			logf("%s: search failed: %v", src.Name(), err)
			errs = append(errs, fmt.Errorf("%s: %w", src.Name(), err))
			continue
		}
		if len(candidates) == 0 {
			logf("%s: nothing for this title", src.Name())
			continue
		}

		used := 0
		for _, cand := range candidates {
			if used >= tries {
				break
			}
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if cand.ID == "" && skip[candidateKey(cand.Source, cand.Release)] {
				// Same file listed again for another language; it still
				// can't be translated, so don't pay to download it twice.
				logf("%s: %s: already downloaded and can't be translated, skipping", src.Name(), cand.Release)
				continue
			}
			data, err := await(ctx, cand.Fetch)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return Result{}, ctxErr
				}
				// e.g. a season pack without our episode. costs a download
				// but not worth stopping for.
				logf("%s: %s: %v", src.Name(), cand.Release, err)
				errs = append(errs, err)
				used++
				continue
			}
			used++

			if c.Speech == nil {
				return Result{Data: data, Source: cand.Source, Release: cand.Release,
					ID: cand.ID, Lang: q.Lang}, nil
			}
			res, fixed, err := c.Speech.Match(data)
			if err != nil {
				logf("%s: %s: %v", src.Name(), cand.Release, err)
				errs = append(errs, err)
				continue
			}
			if !res.Matched() {
				logf("%s: %s: rejected (sigma %.1f, need %.1f; %.0f%% hit vs %.0f%% by chance)",
					src.Name(), cand.Release, res.Sigma(), MinSigma, 100*res.HitRate, 100*res.Baseline)
				continue
			}
			logf("%s: %s: ok (sigma %.1f, %.0f%% hit vs %.0f%% by chance), %s",
				src.Name(), cand.Release, res.Sigma(), 100*res.HitRate, 100*res.Baseline, res.Correction())
			return Result{Data: fixed, Source: cand.Source, Release: cand.Release,
				Sync: res, ID: cand.ID, Lang: q.Lang}, nil
		}
	}

	if len(errs) > 0 {
		return Result{}, fmt.Errorf("%w (%v)", ErrNoMatch, errors.Join(errs...))
	}
	return Result{}, ErrNoMatch
}
