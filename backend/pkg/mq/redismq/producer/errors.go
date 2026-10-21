package producer

import (
	"errors"
	"fmt"
)

var (
	// ErrBulkSendFailed is returned when at least one message of a batch could
	// not be published. Error() of the returned error includes the index.
	ErrBulkSendFailed = errors.New("bulk send failed")
	// ErrClosed is returned when the producer is used after Close.
	ErrClosed = errors.New("mq: producer is closed")
	// ErrNotConnected is returned when Publish is called before Connect.
	ErrNotConnected = errors.New("mq: producer is not connected")
)

// publishError wraps the sentinel with the failing index and the cause.
type publishError struct {
	index int
	total int
	err   error
}

func (e *publishError) Error() string {
	return fmt.Sprintf("bulk send failed at %dth message (of %d): %v", e.index, e.total, e.err)
}

func (e *publishError) Unwrap() error { return e.err }

// Is lets errors.Is(err, ErrBulkSendFailed) keep working.
func (e *publishError) Is(target error) bool { return target == ErrBulkSendFailed }
