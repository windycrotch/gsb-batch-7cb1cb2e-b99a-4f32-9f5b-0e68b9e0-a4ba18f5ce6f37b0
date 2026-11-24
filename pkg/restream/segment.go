package restream

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/shaunschembri/restreamer/pkg/restream/provider"
	"github.com/shaunschembri/restreamer/pkg/restream/request"
)

const decrypterBuffer = 32768

// getSegments runs the pull/decrypt/write pipeline. Every iteration handles
// exactly one segment with a decrypter built from that segment's own key
// information, so encryption state can never carry over from one segment to
// the next. The goroutine exits when the context is cancelled, the input
// channel is drained and closed, or the stream fails; in the failure case
// the error is handed back to Start through the errors channel.
func (r *Restream) getSegments(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case segment, ok := <-r.segments:
			if !ok {
				// The producer closed the queue after delivering every
				// segment of a finished VOD playlist: drain is complete.
				return
			}
			if err := r.writeSegment(ctx, segment); err != nil {
				// Cancellation is a clean stop, not a stream failure: do not
				// push an error that could mask the context cancellation.
				if ctx.Err() != nil {
					return
				}
				select {
				case r.errors <- err:
				default:
				}
				return
			}
		}
	}
}

func (r *Restream) writeSegment(ctx context.Context, segment provider.Segment) error {
	// Never start new work for a cancelled context: no key fetch, no segment
	// request and no further writes are issued.
	if err := ctx.Err(); err != nil {
		return nil
	}

	if r.Writer == nil {
		return fmt.Errorf("stopping streaming as writer is nil")
	}

	// Build a fresh decrypter per segment from the segment's own key and IV.
	// Plain segments get a nil decrypter and are written verbatim; encrypted
	// segments never reuse a previous segment's key. An initialization
	// failure aborts the stream before any undecrypted bytes are written.
	decrypter, err := newDecrypter(ctx, segment, r.UserAgent)
	if err != nil {
		return err
	}

	req := request.New(r.UserAgent)
	response, err := req.Do(ctx, segment.URL)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()

	reader := bufio.NewReaderSize(response.Body, r.ReadBufferSize)
	startTime := time.Now()
	if _, err := reader.Peek(r.ReadBufferSize); err != nil {
		if !errors.Is(err, io.EOF) {
			return fmt.Errorf("error filling buffer: %w", err)
		}
	}
	r.currentBandwidth.Store(uint32(float64(reader.Buffered()*8) / time.Since(startTime).Seconds()))

	writer := NewStreamWriter(ctx, r.Writer)
	buffer := make([]byte, decrypterBuffer)

	// Read the segment one block-aligned chunk at a time. The final flag is
	// derived from the reader's own EOF semantics: ReadFull returns plain
	// io.EOF only when zero bytes were read at the true end of the body, and
	// ErrUnexpectedEOF on the last short read. Any earlier short read is
	// followed by more data, so it is never treated as the final chunk.
	for {
		bytesRead, readErr := io.ReadFull(reader, buffer)
		atBodyEnd := errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF)
		final := false
		if atBodyEnd {
			// Distinguish the last chunk from an intermediate short read by
			// checking whether any further bytes can be read.
			if reader.Buffered() > 0 {
				final = false
			} else {
				one := make([]byte, 1)
				_, probeErr := reader.Read(one)
				switch {
				case errors.Is(probeErr, io.EOF):
					final = true
				case probeErr == nil:
					// More data exists: put the byte back and continue reading.
					if err := reader.UnreadByte(); err != nil {
						return fmt.Errorf("error rewinding stream: %w", err)
					}
				default:
					return fmt.Errorf("error reading stream: %w", probeErr)
				}
			}
		}

		if bytesRead > 0 {
			decryptedPayload, err := decryptSegment(decrypter, buffer[:bytesRead], final)
			if err != nil {
				return fmt.Errorf("cannot decrypt: %w", err)
			}

			bytesWritten, err := writer.Write(decryptedPayload)
			if err != nil {
				return fmt.Errorf("error writing output: %w", err)
			}

			r.streamedBytes.Add(int64(bytesWritten))
		}

		if readErr != nil {
			if final {
				break
			}
			if !atBodyEnd {
				return fmt.Errorf("error reading stream: %w", readErr)
			}
			// Short read with more data coming: keep filling the next chunk.
		}

		// Check the context between chunks so an in-flight segment returns
		// promptly instead of draining the whole body after cancellation.
		if err := ctx.Err(); err != nil {
			return nil
		}
	}

	return nil
}

// decryptSegment applies the segment-scoped decrypter, or passes plaintext
// through unchanged when the segment has no key.
func decryptSegment(decrypter decrypter, payload []byte, final bool) ([]byte, error) {
	if decrypter == nil {
		return payload, nil
	}

	decryptedPayload, err := decrypter.decrypt(payload, final)
	if err != nil {
		return nil, fmt.Errorf("[%s] %w", decrypter.info(), err)
	}

	return decryptedPayload, nil
}

type writerCtx struct {
	ctx    context.Context
	writer io.Writer
}

func (w *writerCtx) Write(p []byte) (int, error) {
	// Never write into a request whose context is already done, e.g. an HTTP
	// handler whose client disconnected or a download whose timeout fired.
	if err := w.ctx.Err(); err != nil {
		return 0, fmt.Errorf("context error: %w", err)
	}

	n, err := w.writer.Write(p)
	if err != nil {
		return n, fmt.Errorf("write error: %w", err)
	}

	return n, nil
}

func NewStreamWriter(ctx context.Context, writer io.Writer) io.Writer {
	return &writerCtx{ctx: ctx, writer: writer}
}
