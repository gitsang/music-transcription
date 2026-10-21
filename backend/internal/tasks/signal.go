package tasks

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// SignalContext returns a context cancelled on SIGTERM or SIGINT. Cancelling it
// is what makes Work/Dispatch stop pulling messages, let the current pipeline
// child be terminated (TERM, then KILL for the whole process group) and leave
// unclaimed work in the durable database for the next process.
func SignalContext(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT, os.Interrupt)
}
