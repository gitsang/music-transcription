package tasks

import (
	"context"
	"syscall"
	"testing"
	"time"
)

func TestSignalContextCancelsOnSIGTERM(t *testing.T) {
	ctx, stop := SignalContext(context.Background())
	defer stop()
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
}

func TestSignalContextAcceptsNilParent(t *testing.T) {
	ctx, stop := SignalContext(nil)
	defer stop()
	if ctx.Err() != nil {
		t.Fatal("a fresh context must not be cancelled")
	}
}
