package forwarder

import (
	"context"
	"time"
)

// SegmentForwardRetryConfig retry configuration for segment forwarding operations
type SegmentForwardRetryConfig struct {
	// MaxAttempts max number of attempts for one operation, including the first try
	MaxAttempts int
	// InitBackoff wait time before the first retry
	InitBackoff time.Duration
	// MaxBackoff max wait time between retries
	MaxBackoff time.Duration
}

/*
DefaultSegmentForwardRetryConfig default retry configuration for segment forwarding

	@returns the default configuration
*/
func DefaultSegmentForwardRetryConfig() SegmentForwardRetryConfig {
	return SegmentForwardRetryConfig{
		MaxAttempts: 3,
		InitBackoff: time.Millisecond * 100,
		MaxBackoff:  time.Second * 2,
	}
}

// normalized fill in sane values for unset fields
func (c SegmentForwardRetryConfig) normalized() SegmentForwardRetryConfig {
	if c.MaxAttempts < 1 {
		c.MaxAttempts = 1
	}
	if c.InitBackoff <= 0 {
		c.InitBackoff = time.Millisecond * 50
	}
	if c.MaxBackoff < c.InitBackoff {
		c.MaxBackoff = c.InitBackoff
	}
	return c
}

/*
runSegmentOpWithRetry execute an operation with bounded retries and exponential backoff.

The operation is attempted at most `cfg.MaxAttempts` times. The function returns:

  - nil once the operation succeeds

  - the operation's last error if all attempts are exhausted

  - the context error if the context is cancelled / times out in between

    @param ctxt context.Context - execution context; cancellation aborts the retry loop
    @param cfg SegmentForwardRetryConfig - retry configuration
    @param op func() error - the operation to execute
    @returns the final outcome of the operation
*/
func runSegmentOpWithRetry(
	ctxt context.Context, cfg SegmentForwardRetryConfig, op func() error,
) error {
	cfg = cfg.normalized()

	var lastErr error
	backoff := cfg.InitBackoff
	for attempt := 0; attempt < cfg.MaxAttempts; attempt++ {
		if err := ctxt.Err(); err != nil {
			return err
		}
		if lastErr = op(); lastErr == nil {
			return nil
		}
		if attempt+1 >= cfg.MaxAttempts {
			break
		}
		// Wait before the next attempt, but stay responsive to cancellation
		timer := time.NewTimer(backoff)
		select {
		case <-ctxt.Done():
			timer.Stop()
			return ctxt.Err()
		case <-timer.C:
		}
		backoff *= 2
		if backoff > cfg.MaxBackoff {
			backoff = cfg.MaxBackoff
		}
	}
	return lastErr
}
