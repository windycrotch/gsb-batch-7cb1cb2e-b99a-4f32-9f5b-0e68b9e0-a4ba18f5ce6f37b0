package uploader

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/OmGuptaIND/cloud"
)

// MaxBufferSize is the size of a single upload part.
var MaxBufferSize = int64(5 * 1024 * 1024) // 5MB

type Uploader struct {
	id  string
	ctx context.Context
	wg  *sync.WaitGroup

	recordingId *string
	storagePath string

	reader *bufio.Reader
	client cloud.CloudClient

	partNumber int

	completedMtx   *sync.Mutex
	completedParts []*cloud.CloudUploadPartReponse
	partErr        error

	// stopMtx serializes Stop calls so concurrent callers share a single
	// finalization. stopResult caches the successful result so repeated
	// Stop calls are idempotent; failures are not cached and can be retried.
	stopMtx    sync.Mutex
	stopResult *cloud.CloudUploadPartCompleted

	buffer []byte
}

// NewUploader creates a new Uploader instance. Start must be called exactly
// once after NewUploader returns, before Stop is called.
func NewUploader(ctx context.Context, reader *bufio.Reader, recordingId *string) (*Uploader, error) {
	uploadCtx := context.WithoutCancel(ctx)

	cloudClient := cloud.GetClient(&uploadCtx)

	if cloudClient == nil {
		return nil, fmt.Errorf("no cloud client found in context")
	}

	storagePath := fmt.Sprintf("recording_%s.mp4", *recordingId)

	uploaderId, err := cloudClient.CreateMultipartUpload(&storagePath)

	if err != nil {
		return nil, err
	}

	uploader := &Uploader{
		ctx:         uploadCtx,
		id:          *uploaderId,
		recordingId: recordingId,
		storagePath: storagePath,

		wg: &sync.WaitGroup{},

		client: cloudClient,
		reader: reader,

		partNumber: 1,

		completedMtx:   &sync.Mutex{},
		completedParts: make([]*cloud.CloudUploadPartReponse, 0),

		buffer: make([]byte, MaxBufferSize),
	}

	// Account for the Start loop up front so a concurrent Stop can never
	// observe an empty WaitGroup while Start is still being scheduled.
	uploader.wg.Add(1)

	return uploader, nil
}

// GetContext returns the context of the Uploader.
func (u *Uploader) GetContext() context.Context {
	return u.ctx
}

// GetID returns the ID of the Uploader.
func (u *Uploader) GetID() string {
	return u.id
}

// GetObjectKey returns the object key.
func (u *Uploader) GetObjectKey() *string {
	key := u.storagePath

	return &key
}

// Wait waits for the Uploader to finish.
func (u *Uploader) Wait() {
	u.wg.Wait()
}

// AddPart adds a part to the Uploader.
func (u *Uploader) addCompletedPart(part *cloud.CloudUploadPartReponse) {
	u.completedMtx.Lock()
	defer u.completedMtx.Unlock()

	u.completedParts = append(u.completedParts, part)
}

// setPartErr records the first part upload error, so Stop can surface it
// instead of silently completing a recording with missing parts.
func (u *Uploader) setPartErr(err error) {
	u.completedMtx.Lock()
	defer u.completedMtx.Unlock()

	if u.partErr == nil {
		u.partErr = err
	}
}

// getPartErr returns the first part upload error, if any.
func (u *Uploader) getPartErr() error {
	u.completedMtx.Lock()
	defer u.completedMtx.Unlock()

	return u.partErr
}

// Start starts the Uploader. It reads from the recording until EOF, uploading
// a part every time the buffer fills up, and uploads the remaining buffered
// data as the final part once EOF is reached.
func (u *Uploader) Start() error {
	defer func() {
		log.Println("Start Uploader is done, Getting out of Start Uploader")
		u.wg.Done()
	}()

	if u.recordingId == nil {
		log.Println("No recording found to upload.")
		return fmt.Errorf("no recording found to upload: %s", u.GetID())
	}

	bytesRead := 0

	for {
		n, err := u.reader.Read(u.buffer[bytesRead:])

		if err != nil && err != io.EOF && n == 0 {
			return fmt.Errorf("failed to read from reader: %v", err)
		}

		bytesRead += n

		if bytesRead >= int(MaxBufferSize) || (err == io.EOF && bytesRead > 0) {
			log.Println("New Part Started", u.partNumber, " Size", bytesRead, " Req_Size", MaxBufferSize)
			tempBuffer := make([]byte, bytesRead)
			copy(tempBuffer, u.buffer[:bytesRead])

			partInput := &cloud.CloudUploadPartInput{
				UploadId:    u.GetID(),
				StoragePath: u.GetObjectKey(),
				Buffer:      &tempBuffer,
				PartNumber:  u.partNumber,
			}

			u.wg.Add(1)
			go func() {
				defer u.wg.Done()
				part, err := u.uploadPart(partInput)

				if err != nil {
					log.Println("Failed to upload part", partInput.PartNumber, err)
					u.setPartErr(fmt.Errorf("part %d: %w", partInput.PartNumber, err))
					return
				}

				u.addCompletedPart(part)
			}()

			u.partNumber++
			bytesRead = 0
		}

		if err == io.EOF {
			log.Println("EOF reached")
			break
		}
	}

	return nil
}

// Stop waits until every remaining byte of the recording has been uploaded
// and then commits the multipart upload.
//
// Stop is safe to call repeatedly and concurrently: concurrent callers wait
// for the in-flight finalization and share its outcome. A successful result
// is cached and returned to every subsequent caller; a failure is not cached,
// so Stop can be retried and will attempt the completion again.
func (u *Uploader) Stop() (*cloud.CloudUploadPartCompleted, error) {
	u.stopMtx.Lock()
	defer u.stopMtx.Unlock()

	if u.stopResult != nil {
		log.Println("Uploader already stopped, returning cached result")
		return u.stopResult, nil
	}

	log.Println("Stopping uploader...")

	// Wait for the read loop and all in-flight part uploads, so every
	// remaining byte of the recording is uploaded before completing.
	u.wg.Wait()

	// A failed part upload must never be treated as a success.
	if err := u.getPartErr(); err != nil {
		return nil, fmt.Errorf("cannot complete upload, a part failed to upload: %w", err)
	}

	resp, err := u.completeUpload()

	if err != nil {
		return nil, err
	}

	u.stopResult = resp

	return resp, nil
}

// completeUpload completes the upload.
func (u *Uploader) completeUpload() (*cloud.CloudUploadPartCompleted, error) {
	log.Println("Completing upload...", len(u.completedParts))

	resp, err := u.client.CompletePartUpload(&cloud.CloudUploadPartInput{
		UploadId:    u.GetID(),
		StoragePath: u.GetObjectKey(),
		Parts:       &u.completedParts,
	})

	if err != nil {
		return nil, fmt.Errorf("failed to complete multipart upload: %v", err)
	}

	if resp == nil || resp.Recording_Url == nil {
		return nil, fmt.Errorf("completed multipart upload but got no recording url back")
	}

	log.Println("Multipart upload completed", u.GetID(), " Completed Parts Count", len(u.completedParts))

	return &cloud.CloudUploadPartCompleted{
		Recording_Url: resp.Recording_Url,
	}, nil
}

// Upload uploads the recording to the cloud.
func (u *Uploader) uploadPart(input *cloud.CloudUploadPartInput) (*cloud.CloudUploadPartReponse, error) {
	log.Println("Uploading part to cloud...", input.PartNumber)

	partResp, err := u.client.UploadPart(input)

	if err != nil {
		return nil, err
	}

	log.Println("Part uploaded successfully", input.PartNumber, " ETag", partResp.ETag)

	return partResp, nil
}
