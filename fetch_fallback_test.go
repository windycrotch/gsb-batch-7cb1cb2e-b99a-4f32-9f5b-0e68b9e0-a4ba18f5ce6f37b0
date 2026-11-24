package subtitles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/m314dev/m314ss/srt"
	"github.com/m314dev/subapi"
)

// confirmedPair builds speech and a subtitle whose cues start five seconds
// after the speech onsets: Match accepts it and shifts it by -5s.
func confirmedPair(until float64) (*Speech, []byte) {
	rng := rand.New(rand.NewSource(1))
	onsets := speech(rng, until)
	var starts []float64
	for _, o := range onsets {
		if rng.Float64() < 0.75 {
			starts = append(starts, o+5)
		}
	}
	return &Speech{Onsets: onsets, Windows: [][2]float64{{0, until}}}, srtFromStarts(starts)
}

func srtFromStarts(starts []float64) []byte {
	var b []byte
	for i, s := range starts {
		b = fmt.Appendf(b, "%d\n%s --> %s\nline %d\n\n", i+1, stampTime(s), stampTime(s+1.5), i+1)
	}
	return b
}

func stampTime(x float64) string {
	ms := int(math.Round(x * 1000))
	return fmt.Sprintf("%02d:%02d:%02d,%03d", ms/3600000, ms/60000%60, ms/1000%60, ms%1000)
}

// otherFile is a subtitle timed like some other video: none of its cues land
// on our speech, so it can't pass Match and isn't timed like the original.
func otherFile() []byte {
	var starts []float64
	for i := range 120 {
		starts = append(starts, 1+3.1*float64(i))
	}
	return srtFromStarts(starts)
}

// fakeSource is a subapi.Source over canned candidates, counting downloads.
type fakeSource struct {
	name string
	find func(context.Context, subapi.Query) ([]subapi.Candidate, error)

	mu       sync.Mutex
	download map[string]int
}

func (s *fakeSource) Name() string { return s.name }

func (s *fakeSource) Find(ctx context.Context, q subapi.Query) ([]subapi.Candidate, error) {
	return s.find(ctx, q)
}

func (s *fakeSource) downloads(release string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.download[release]
}

func (s *fakeSource) count(release string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.download == nil {
		s.download = map[string]int{}
	}
	s.download[release]++
}

// cand returns a candidate that answers with data and counts the download.
func (s *fakeSource) cand(release, id string, data []byte) subapi.Candidate {
	return subapi.Candidate{
		Source:  s.name,
		Release: release,
		ID:      id,
		Fetch: func(context.Context) ([]byte, error) {
			s.count(release)
			return data, nil
		},
	}
}

// fakeTranslator is a Translator over canned answers, recording the ids it was
// asked about.
type fakeTranslator struct {
	wait     func(context.Context, string) (subapi.Job, error)
	download func(context.Context, subapi.Job) ([]byte, error)

	mu    sync.Mutex
	asked []string
}

func (t *fakeTranslator) TranslateEp(ctx context.Context, nID, targetLang, tone string, season, episode int) (subapi.Job, error) {
	t.mu.Lock()
	t.asked = append(t.asked, nID)
	t.mu.Unlock()
	return subapi.Job{ID: "job-" + nID, RequestID: "job-" + nID}, nil
}

func (t *fakeTranslator) WaitTranslation(ctx context.Context, id string, _ time.Duration) (subapi.Job, error) {
	if t.wait != nil {
		return t.wait(ctx, id)
	}
	return subapi.Job{ID: id, RequestID: id, DownloadReady: true, DownloadURL: "https://example.invalid/" + id + ".srt"}, nil
}

func (t *fakeTranslator) DownloadTranslation(ctx context.Context, job subapi.Job) ([]byte, error) {
	if t.download != nil {
		return t.download(ctx, job)
	}
	return nil, nil
}

func (t *fakeTranslator) ids() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.asked...)
}

// emptyQuerySource answers nothing, for the tests that only care about the
// target language.
func emptyQuerySource(name string) *fakeSource {
	return &fakeSource{name: name, find: func(context.Context, subapi.Query) ([]subapi.Candidate, error) {
		return nil, nil
	}}
}

// langSource answers with the candidates registered for a language.
func langSource(name string, byLang map[string][]subapi.Candidate) *fakeSource {
	return &fakeSource{name: name, find: func(_ context.Context, q subapi.Query) ([]subapi.Candidate, error) {
		return byLang[strings.ToUpper(q.Lang)], nil
	}}
}

// The normal path is untouched: the first candidate the audio agrees with is
// returned as is, and no candidate after it is downloaded.
func TestFetchReturnsFirstVerifiedCandidate(t *testing.T) {
	speech, good := confirmedPair(1200)
	var secondDownloads int
	src := emptyQuerySource("fake")
	src.find = func(context.Context, subapi.Query) ([]subapi.Candidate, error) {
		return []subapi.Candidate{
			src.cand("junk", "1", otherFile()),
			{
				Source: "fake", Release: "good", ID: "2",
				Fetch: func(context.Context) ([]byte, error) {
					return good, nil
				},
			},
			{
				Source: "fake", Release: "later", ID: "3",
				Fetch: func(context.Context) ([]byte, error) {
					secondDownloads++
					return good, nil
				},
			},
		}, nil
	}

	c := Client{Sources: []subapi.Source{src}, Speech: speech}
	res, err := c.Fetch(context.Background(), subapi.Query{Lang: "AR"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Release != "good" || res.ID != "2" || res.Lang != "AR" {
		t.Fatalf("got %s/%s lang %s, want good/2 lang AR", res.Release, res.ID, res.Lang)
	}
	if !res.Sync.Matched() || res.Translated {
		t.Fatalf("matched=%v translated=%v", res.Sync.Matched(), res.Translated)
	}
	if _, _, err := speech.Match(good); err != nil {
		t.Fatal(err)
	}
	if secondDownloads != 0 {
		t.Fatalf("downloaded the candidate after the accepted one")
	}
}

// A translation that keeps the original's timing is returned shifted like the
// original and carries the language it came from.
func TestFetchTranslationFallbackSucceeds(t *testing.T) {
	speech, good := confirmedPair(1200)
	src := langSource("fake", map[string][]subapi.Candidate{
		"EN": {{
			Source: "fake", Release: "english", ID: "n1",
			Fetch: func(context.Context) ([]byte, error) { return good, nil },
		}},
	})
	translated := srtFromStarts(speechStarts(good))
	tr := &fakeTranslator{download: func(context.Context, subapi.Job) ([]byte, error) {
		return translated, nil
	}}

	c := Client{Sources: []subapi.Source{src}, Speech: speech, TranslateWith: tr,
		FallbackLangs: []string{"EN"}}
	res, err := c.Fetch(context.Background(), subapi.Query{Lang: "AR"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Translated || res.Lang != "AR" || res.FromLang != "EN" {
		t.Fatalf("translated=%v lang=%s from=%s, want true/AR/EN", res.Translated, res.Lang, res.FromLang)
	}
	// The original was five seconds late, so the translation is shifted by
	// the same correction.
	if got := srt.Starts(srt.Parse(res.Data)); len(got) == 0 || math.Abs(got[0]-(speechStarts(good)[0]-5)) > 0.2 {
		t.Fatalf("translation wasn't retimed like the original: %v", got)
	}
}

// A cancel during a download stops the request at once, without moving on to
// the next candidate, and the error is a cancel, not ErrNoMatch.
func TestFetchCancelDuringDownload(t *testing.T) {
	speech, good := confirmedPair(1200)
	started := make(chan struct{})
	fetchDone := make(chan struct{})
	var laterStarted int
	src := emptyQuerySource("fake")
	src.find = func(context.Context, subapi.Query) ([]subapi.Candidate, error) {
		return []subapi.Candidate{
			{
				Source: "fake", Release: "slow", ID: "1",
				Fetch: func(ctx context.Context) ([]byte, error) {
					close(started)
					<-ctx.Done()
					close(fetchDone)
					return nil, ctx.Err()
				},
			},
			{
				Source: "fake", Release: "later", ID: "2",
				Fetch: func(context.Context) ([]byte, error) {
					laterStarted++
					return good, nil
				},
			},
		}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := Client{Sources: []subapi.Source{src}, Speech: speech}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Fetch(ctx, subapi.Query{Lang: "AR"})
		errCh <- err
	}()

	<-started
	cancel()
	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrNoMatch) {
		t.Fatal("a cancel was reported as no match")
	}
	<-fetchDone // the aborted download goroutine has let go
	if laterStarted != 0 {
		t.Fatal("downloaded the next candidate after the cancel")
	}
}

// A cancel while waiting for a paid translation stops the wait and shows up as
// a cancel, not as "no subtitle in any language".
func TestFetchCancelDuringTranslationWait(t *testing.T) {
	speech, good := confirmedPair(1200)
	started := make(chan struct{})
	src := langSource("fake", map[string][]subapi.Candidate{
		"EN": {func() subapi.Candidate {
			s := &fakeSource{name: "fake"}
			return s.cand("english", "n1", good)
		}()},
	})
	tr := &fakeTranslator{wait: func(ctx context.Context, id string) (subapi.Job, error) {
		close(started)
		<-ctx.Done()
		return subapi.Job{}, ctx.Err()
	}}

	ctx, cancel := context.WithCancel(context.Background())
	c := Client{Sources: []subapi.Source{src}, Speech: speech, TranslateWith: tr, FallbackLangs: []string{"EN"}}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Fetch(ctx, subapi.Query{Lang: "AR"})
		errCh <- err
	}()

	<-started
	cancel()
	err := <-errCh
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrNoMatch) {
		t.Fatal("a cancel was reported as no match")
	}
	if got := tr.ids(); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("translation ids = %v, want [n1]", got)
	}
}

// A source or download that ignores its context must still not keep a canceled
// caller waiting: the request returns as soon as ctx is done.
func TestFetchCancelDoesNotWaitOnContextIgnoringSource(t *testing.T) {
	speech, _ := confirmedPair(1200)
	started := make(chan struct{})
	unblock := make(chan struct{})
	defer close(unblock)
	src := emptyQuerySource("stuck")
	src.find = func(context.Context, subapi.Query) ([]subapi.Candidate, error) {
		close(started)
		<-unblock // simulates an HTTP call that doesn't watch ctx
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := Client{Sources: []subapi.Source{src}, Speech: speech}
	errCh := make(chan error, 1)
	go func() {
		_, err := c.Fetch(ctx, subapi.Query{Lang: "AR"})
		errCh <- err
	}()

	<-started
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Fetch stayed blocked on a source that ignores its context")
	}
}

// A verified candidate with no id can't be translated. Trying the next
// language lists it again; it must not be downloaded a second time.
func TestFetchSkipsAlreadyDownloadedIDLessCandidate(t *testing.T) {
	speech, good := confirmedPair(1200)
	src := &fakeSource{name: "fake"}
	noID := src.cand("no-id", "", good)
	withID := src.cand("with-id", "n2", good)
	src.find = func(_ context.Context, q subapi.Query) ([]subapi.Candidate, error) {
		switch strings.ToUpper(q.Lang) {
		case "AR":
			return nil, nil
		case "EN":
			return []subapi.Candidate{noID}, nil
		default: // FR, and whatever comes after
			return []subapi.Candidate{noID, withID}, nil
		}
	}
	src.download = map[string]int{}

	translated := srtFromStarts(append([]float64(nil), speechStarts(good)...))
	tr := &fakeTranslator{download: func(context.Context, subapi.Job) ([]byte, error) {
		return translated, nil
	}}

	c := Client{Sources: []subapi.Source{src}, Speech: speech, TranslateWith: tr,
		FallbackLangs: []string{"EN", "FR"}}
	res, err := c.Fetch(context.Background(), subapi.Query{Lang: "AR"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Translated || res.FromLang != "FR" || res.Lang != "AR" {
		t.Fatalf("translated=%v from=%s lang=%s, want a translation from FR to AR",
			res.Translated, res.FromLang, res.Lang)
	}
	if res.Release != "with-id" {
		t.Fatalf("release = %s, want with-id", res.Release)
	}
	if n := src.downloads("no-id"); n != 1 {
		t.Fatalf("id-less candidate downloaded %d times, want 1", n)
	}
	if n := src.downloads("with-id"); n != 1 {
		t.Fatalf("translatable candidate downloaded %d times, want 1", n)
	}
	if got := tr.ids(); len(got) != 1 || got[0] != "n2" {
		t.Fatalf("translation ids = %v, want [n2]", got)
	}
}

// A paid translation timed like another file is unusable, but the original was
// verified and still fits this video, so it is returned instead of failing.
func TestFetchTranslationTimingMismatchFallsBackToOriginal(t *testing.T) {
	speech, good := confirmedPair(1200)
	src := langSource("fake", map[string][]subapi.Candidate{
		"EN": {{
			Source: "fake", Release: "english", ID: "n1",
			Fetch: func(context.Context) ([]byte, error) { return good, nil },
		}},
	})
	tr := &fakeTranslator{download: func(context.Context, subapi.Job) ([]byte, error) {
		return otherFile(), nil
	}}

	c := Client{Sources: []subapi.Source{src}, Speech: speech, TranslateWith: tr,
		FallbackLangs: []string{"EN"}}
	res, err := c.Fetch(context.Background(), subapi.Query{Lang: "AR"})
	if err != nil {
		t.Fatalf("a mistimed translation should fall back to the original, got %v", err)
	}
	if res.Translated {
		t.Fatal("returned the mistimed translation as if it had been accepted")
	}
	if res.Lang != "EN" || res.FromLang != "" {
		t.Fatalf("lang=%s from=%s, want the verified EN subtitle", res.Lang, res.FromLang)
	}
	_, want, err := speech.Match(good)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.Data, want) {
		t.Fatal("did not return the verified, retimed original")
	}
	if got := tr.ids(); len(got) != 1 || got[0] != "n1" {
		t.Fatalf("translation ids = %v, want [n1]", got)
	}
}

// Concurrent requests don't share cancellation: one caller hanging up leaves
// the others working and returning their own results.
func TestFetchConcurrentCancellationIsolated(t *testing.T) {
	const n = 6
	speech, good := confirmedPair(1200)
	src := langSource("fake", map[string][]subapi.Candidate{
		"EN": {{
			Source: "fake", Release: "english", ID: "n1",
			Fetch: func(context.Context) ([]byte, error) { return good, nil },
		}},
	})

	started := make(chan struct{}, n)
	release := make(chan struct{})
	tr := &fakeTranslator{
		wait: func(ctx context.Context, id string) (subapi.Job, error) {
			started <- struct{}{}
			select {
			case <-release:
				return subapi.Job{ID: id, DownloadReady: true, DownloadURL: "https://example.invalid/x.srt"}, nil
			case <-ctx.Done():
				return subapi.Job{}, ctx.Err()
			}
		},
		download: func(context.Context, subapi.Job) ([]byte, error) {
			return srtFromStarts(speechStarts(good)), nil
		},
	}
	c := Client{Sources: []subapi.Source{src}, Speech: speech, TranslateWith: tr,
		FallbackLangs: []string{"EN"}}

	ctxs := make([]context.Context, n)
	cancels := make([]context.CancelFunc, n)
	errs := make([]error, n)
	results := make([]Result, n)
	done := make([]chan struct{}, n)
	var wg sync.WaitGroup
	for i := range n {
		ctxs[i], cancels[i] = context.WithCancel(context.Background())
		defer cancels[i]()
		done[i] = make(chan struct{})
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done[i])
			results[i], errs[i] = c.Fetch(ctxs[i], subapi.Query{Lang: "AR"})
		}()
	}

	// All n are blocked in the translation wait before anything is canceled.
	for range n {
		<-started
	}
	cancels[0]()
	<-done[0] // request 0 comes back while the rest still wait on release
	if !errors.Is(errs[0], context.Canceled) {
		t.Fatalf("canceled request: got %v, want context.Canceled", errs[0])
	}
	close(release)
	wg.Wait()
	for i := 1; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("request %d: %v", i, errs[i])
		}
		if !results[i].Translated || results[i].Lang != "AR" {
			t.Fatalf("request %d: translated=%v lang=%s", i, results[i].Translated, results[i].Lang)
		}
	}
}

func speechStarts(data []byte) []float64 {
	return srt.Starts(srt.Parse(data))
}

// The HTTP layer tells a canceled request from a genuine no-match.
func TestHandlerCancellationIsNotNotFound(t *testing.T) {
	src := emptyQuerySource("fake")
	h := Handler([]subapi.Source{src}, nil)

	body := func() *strings.Reader {
		b, _ := json.Marshal(Request{Lang: "AR"})
		return strings.NewReader(string(b))
	}

	// Genuinely nothing found.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/subtitle", body())
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("no match: status %d, want 404", rec.Code)
	}

	// Caller went away.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/subtitle", body()).WithContext(ctx)
	h.ServeHTTP(rec, req)
	if rec.Code == http.StatusNotFound {
		t.Fatal("a canceled request was answered with 404 no-match")
	}
	if rec.Code != http.StatusRequestTimeout {
		t.Fatalf("canceled: status %d, want %d", rec.Code, http.StatusRequestTimeout)
	}
	if !strings.Contains(rec.Body.String(), "canceled") {
		t.Fatalf("body %q doesn't say canceled", rec.Body.String())
	}
}
