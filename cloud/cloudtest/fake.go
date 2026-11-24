// Package cloudtest provides a controllable in-memory fake of the
// cloud.CloudClient interface, so upload and finalization logic can be
// tested offline without a real object store.
package cloudtest

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/OmGuptaIND/cloud"
)

// FakeClient is an in-memory fake of cloud.CloudClient.
//
// It records every call in order, stores uploaded parts per upload id, and
// can be configured to fail specific operations a controlled number of times.
// Completing the same upload id twice is idempotent and yields a single
// object, mirroring real object-store semantics.
type FakeClient struct {
	mu sync.Mutex

	// CreateUploadErr, when non-nil, makes CreateMultipartUpload fail.
	CreateUploadErr error
	// PartErrors fails UploadPart for the given part numbers.
	PartErrors map[int]error
	// CompleteErrors are returned, in order, by the first calls to
	// CompletePartUpload; once exhausted, completes succeed.
	CompleteErrors []error
	// RecordingURL is the url returned by a successful complete. When empty,
	// a url is derived from the storage path.
	RecordingURL string

	calls   []string
	uploads map[string]*fakeUpload
	nextID  int
}

type fakeUpload struct {
	storagePath string
	parts       map[int][]byte
	completed   bool
	url         string
}

var _ cloud.CloudClient = (*FakeClient)(nil)

// NewFakeClient creates a ready-to-use fake client.
func NewFakeClient() *FakeClient {
	return &FakeClient{
		uploads: make(map[string]*fakeUpload),
	}
}

// CreateMultipartUpload starts a new fake multipart upload.
func (f *FakeClient) CreateMultipartUpload(storagePath *string) (*string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, "CreateMultipartUpload")

	if f.CreateUploadErr != nil {
		return nil, f.CreateUploadErr
	}

	f.nextID++
	uploadID := fmt.Sprintf("upload-%d", f.nextID)

	f.uploads[uploadID] = &fakeUpload{
		storagePath: *storagePath,
		parts:       make(map[int][]byte),
	}

	return &uploadID, nil
}

// UploadPart stores a part, unless configured to fail for that part number.
func (f *FakeClient) UploadPart(input *cloud.CloudUploadPartInput) (*cloud.CloudUploadPartReponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, fmt.Sprintf("UploadPart(%d)", input.PartNumber))

	if err, ok := f.PartErrors[input.PartNumber]; ok {
		return nil, err
	}

	upload, ok := f.uploads[input.UploadId]
	if !ok {
		return nil, fmt.Errorf("unknown upload id %q", input.UploadId)
	}

	data := make([]byte, len(*input.Buffer))
	copy(data, *input.Buffer)
	upload.parts[input.PartNumber] = data

	etag := fmt.Sprintf("etag-%s-%d", input.UploadId, input.PartNumber)
	partNumber := int64(input.PartNumber)

	return &cloud.CloudUploadPartReponse{
		ETag:       &etag,
		PartNumber: &partNumber,
	}, nil
}

// CompletePartUpload fails while CompleteErrors is non-empty, then completes
// the upload idempotently: completing the same upload id again returns the
// same url without creating a second object.
func (f *FakeClient) CompletePartUpload(input *cloud.CloudUploadPartInput) (*cloud.CloudUploadPartCompleted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, "CompletePartUpload")

	if len(f.CompleteErrors) > 0 {
		err := f.CompleteErrors[0]
		f.CompleteErrors = f.CompleteErrors[1:]
		return nil, err
	}

	upload, ok := f.uploads[input.UploadId]
	if !ok {
		return nil, fmt.Errorf("unknown upload id %q", input.UploadId)
	}

	if !upload.completed {
		url := f.RecordingURL
		if url == "" {
			url = fmt.Sprintf("https://fake-bucket.fake-endpoint/%s", upload.storagePath)
		}

		upload.completed = true
		upload.url = url
	}

	return &cloud.CloudUploadPartCompleted{
		Recording_Url: &upload.url,
	}, nil
}

// UploadFile is a no-op fake.
func (f *FakeClient) UploadFile(fileName *string, filePath string) error {
	return nil
}

// DownloadFile is a no-op fake.
func (f *FakeClient) DownloadFile(fileName *string, downloadPath string) error {
	return nil
}

// Calls returns the ordered log of mutating calls the fake received.
func (f *FakeClient) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	calls := make([]string, len(f.calls))
	copy(calls, f.calls)

	return calls
}

// CompleteCallCount returns how many times CompletePartUpload was invoked.
func (f *FakeClient) CompleteCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, call := range f.calls {
		if call == "CompletePartUpload" {
			count++
		}
	}

	return count
}

// CompletedObjectCount returns how many distinct objects were completed.
func (f *FakeClient) CompletedObjectCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	count := 0
	for _, upload := range f.uploads {
		if upload.completed {
			count++
		}
	}

	return count
}

// CompletedData returns the concatenated parts, in part-number order, of the
// single completed upload. It returns nil when no upload was completed.
func (f *FakeClient) CompletedData() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, upload := range f.uploads {
		if !upload.completed {
			continue
		}

		var data []byte
		for partNumber := 1; ; partNumber++ {
			part, ok := upload.parts[partNumber]
			if !ok {
				break
			}
			data = append(data, part...)
		}

		return data
	}

	return nil
}

// UploadedData returns the concatenation of everything ever uploaded, in
// call order; useful to compare against the recorded source bytes.
func (f *FakeClient) UploadedData() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	var data []byte
	for _, upload := range f.uploads {
		for partNumber := 1; ; partNumber++ {
			part, ok := upload.parts[partNumber]
			if !ok {
				break
			}
			data = append(data, part...)
		}
	}

	return bytes.Clone(data)
}
