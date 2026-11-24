package uploader_test

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/OmGuptaIND/cloud"
	"github.com/OmGuptaIND/cloud/cloudtest"
	"github.com/OmGuptaIND/uploader"
)

// newTestUploader builds an Uploader backed by the fake cloud client and a
// pipe standing in for the recording process stdout. The returned writer
// plays the role of the recorder: closing it signals the end of the
// recording, like the recorder process exiting does.
func newTestUploader(t *testing.T, client *cloudtest.FakeClient) (*uploader.Uploader, *io.PipeWriter) {
	t.Helper()

	reader, writer := io.Pipe()

	ctx := context.WithValue(context.Background(), cloud.ClientKey, client)
	recordingID := "pipeline_test"

	u, err := uploader.NewUploader(ctx, bufio.NewReader(reader), &recordingID)
	if err != nil {
		t.Fatalf("NewUploader failed: %v", err)
	}

	return u, writer
}

// startRecording simulates a recording: data is written through the pipe and
// the writer is closed, which is what happens when the recorder process
// stops and its stdout reaches EOF.
func startRecording(t *testing.T, u *uploader.Uploader, writer *io.PipeWriter, data []byte) {
	t.Helper()

	startDone := make(chan error, 1)
	go func() {
		startDone <- u.Start()
	}()

	if _, err := writer.Write(data); err != nil {
		t.Fatalf("failed to write recording data: %v", err)
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("failed to close recording writer: %v", err)
	}

	if err := <-startDone; err != nil {
		t.Fatalf("uploader Start failed: %v", err)
	}
}

func withSmallParts(t *testing.T, size int64) {
	t.Helper()

	original := uploader.MaxBufferSize
	uploader.MaxBufferSize = size
	t.Cleanup(func() { uploader.MaxBufferSize = original })
}

// TestStopReturnsRecordingURL covers the normal stop: once the recorder
// stops, every remaining byte is uploaded, the parts are committed in order,
// and the recording url is returned.
func TestStopReturnsRecordingURL(t *testing.T) {
	withSmallParts(t, 8)

	client := cloudtest.NewFakeClient()
	u, writer := newTestUploader(t, client)

	data := []byte("recording-data-that-spans-multiple-parts")
	startRecording(t, u, writer, data)

	resp, err := u.Stop()
	if err != nil {
		t.Fatalf("Stop failed: %v", err)
	}

	if resp == nil || resp.Recording_Url == nil {
		t.Fatal("Stop returned no recording url")
	}

	expectedURL := "https://fake-bucket.fake-endpoint/recording_pipeline_test.mp4"
	if *resp.Recording_Url != expectedURL {
		t.Fatalf("unexpected recording url: got %q want %q", *resp.Recording_Url, expectedURL)
	}

	// The whole recording, and nothing else, must have been uploaded.
	if got := client.CompletedData(); string(got) != string(data) {
		t.Fatalf("completed object mismatch: got %q want %q", got, data)
	}

	// Ordering: all parts uploaded before the multipart commit, which ran
	// exactly once.
	calls := client.Calls()
	completeIndex := -1
	for i, call := range calls {
		if call == "CompletePartUpload" {
			completeIndex = i
			break
		}
	}
	if completeIndex == -1 {
		t.Fatal("CompletePartUpload was never called")
	}
	partCount := 0
	for i, call := range calls {
		if strings.HasPrefix(call, "UploadPart(") {
			partCount++
			if i > completeIndex {
				t.Fatalf("part uploaded after completion: %v", calls)
			}
		}
	}
	if partCount != 5 {
		t.Fatalf("expected 5 parts for %d bytes with 8 byte parts, got %d", len(data), partCount)
	}
	if client.CompleteCallCount() != 1 {
		t.Fatalf("expected exactly one completion, got %d", client.CompleteCallCount())
	}
	if client.CompletedObjectCount() != 1 {
		t.Fatalf("expected exactly one completed object, got %d", client.CompletedObjectCount())
	}
}

// TestStopRetryAfterFinalizeFailure covers a stop whose final commit fails:
// the first Stop returns an error and no url, a retry with the same uploader
// succeeds, and the object store ends up with a single complete recording.
func TestStopRetryAfterFinalizeFailure(t *testing.T) {
	client := cloudtest.NewFakeClient()
	client.CompleteErrors = []error{errors.New("transient object store failure")}

	u, writer := newTestUploader(t, client)
	startRecording(t, u, writer, []byte("some recorded bytes"))

	resp, err := u.Stop()
	if err == nil {
		t.Fatal("first Stop should have failed")
	}
	if resp != nil {
		t.Fatalf("failed Stop must not return a result, got %+v", resp)
	}
	if client.CompletedObjectCount() != 0 {
		t.Fatal("no object may be completed after a failed stop")
	}

	// Retry with the same uploader: must succeed and return the url.
	resp, err = u.Stop()
	if err != nil {
		t.Fatalf("retried Stop failed: %v", err)
	}
	if resp == nil || resp.Recording_Url == nil {
		t.Fatal("retried Stop returned no recording url")
	}

	if client.CompleteCallCount() != 2 {
		t.Fatalf("expected 2 complete calls (failure + retry), got %d", client.CompleteCallCount())
	}
	if client.CompletedObjectCount() != 1 {
		t.Fatalf("expected exactly one completed object after retry, got %d", client.CompletedObjectCount())
	}

	// A further Stop is idempotent: same url, no new commit.
	again, err := u.Stop()
	if err != nil {
		t.Fatalf("third Stop failed: %v", err)
	}
	if *again.Recording_Url != *resp.Recording_Url {
		t.Fatalf("idempotent Stop returned a different url: %q vs %q", *again.Recording_Url, *resp.Recording_Url)
	}
	if client.CompleteCallCount() != 2 {
		t.Fatalf("idempotent Stop triggered a new commit, total %d", client.CompleteCallCount())
	}
}

// TestConcurrentStopFinalizesOnce covers concurrent stop requests for the
// same recording: exactly one finalization runs, and every caller observes
// the same successful result.
func TestConcurrentStopFinalizesOnce(t *testing.T) {
	client := cloudtest.NewFakeClient()
	u, writer := newTestUploader(t, client)
	startRecording(t, u, writer, []byte("concurrent stop data"))

	const callers = 8

	results := make([]*cloud.CloudUploadPartCompleted, callers)
	errs := make([]error, callers)

	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = u.Stop()
		}(i)
	}
	wg.Wait()

	var url string
	for i := 0; i < callers; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent Stop %d failed: %v", i, errs[i])
		}
		if results[i] == nil || results[i].Recording_Url == nil {
			t.Fatalf("concurrent Stop %d returned no recording url", i)
		}
		if url == "" {
			url = *results[i].Recording_Url
		} else if *results[i].Recording_Url != url {
			t.Fatalf("concurrent callers got different urls: %q vs %q", *results[i].Recording_Url, url)
		}
	}

	if client.CompleteCallCount() != 1 {
		t.Fatalf("expected exactly one finalization, got %d", client.CompleteCallCount())
	}
	if client.CompletedObjectCount() != 1 {
		t.Fatalf("expected exactly one completed object, got %d", client.CompletedObjectCount())
	}
}

// TestPartUploadFailureIsNotSilent covers a failed part upload: Stop must
// surface the error instead of committing an incomplete recording.
func TestPartUploadFailureIsNotSilent(t *testing.T) {
	withSmallParts(t, 4)

	client := cloudtest.NewFakeClient()
	client.PartErrors = map[int]error{2: errors.New("part 2 upload failed")}

	u, writer := newTestUploader(t, client)
	startRecording(t, u, writer, []byte("0123456789")) // 3 parts of 4 bytes

	resp, err := u.Stop()
	if err == nil {
		t.Fatal("Stop must fail when a part upload failed")
	}
	if resp != nil {
		t.Fatalf("failed Stop must not return a result, got %+v", resp)
	}
	if !strings.Contains(err.Error(), "part 2") {
		t.Fatalf("error should identify the failed part, got: %v", err)
	}
	if client.CompleteCallCount() != 0 {
		t.Fatal("multipart commit must not run when a part is missing")
	}
	if client.CompletedObjectCount() != 0 {
		t.Fatal("no object may be completed when a part upload failed")
	}
}

// TestStopWaitsForInFlightUploads covers the ordering guarantee: Stop does
// not commit until the remaining recording data has been uploaded, even when
// the last bytes arrive while Stop is already waiting.
func TestStopWaitsForInFlightUploads(t *testing.T) {
	client := cloudtest.NewFakeClient()
	u, writer := newTestUploader(t, client)

	startDone := make(chan error, 1)
	go func() {
		startDone <- u.Start()
	}()

	if _, err := writer.Write([]byte("first chunk,")); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	stopDone := make(chan struct{})
	var stopResp *cloud.CloudUploadPartCompleted
	var stopErr error
	go func() {
		defer close(stopDone)
		stopResp, stopErr = u.Stop()
	}()

	// The recorder keeps producing data after Stop was requested; it must
	// still end up in the upload before the commit.
	if _, err := writer.Write([]byte("final chunk")); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}

	if err := <-startDone; err != nil {
		t.Fatalf("uploader Start failed: %v", err)
	}

	<-stopDone

	if stopErr != nil {
		t.Fatalf("Stop failed: %v", stopErr)
	}
	if stopResp == nil || stopResp.Recording_Url == nil {
		t.Fatal("Stop returned no recording url")
	}

	want := "first chunk,final chunk"
	if got := string(client.CompletedData()); got != want {
		t.Fatalf("completed object mismatch: got %q want %q", got, want)
	}
}

// Example of the error surfaced when no cloud client is configured.
func TestNewUploaderWithoutClient(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()

	recordingID := "pipeline_test"
	_, err := uploader.NewUploader(context.Background(), bufio.NewReader(reader), &recordingID)
	if err == nil {
		t.Fatal("expected an error when no cloud client is in the context")
	}
}
