package tasks

import (
	"context"
	"testing"
)

// The dispatcher publishes from a process that never runs Work(), so the
// producer must be connected during Runner construction. This regression test
// guards the real deployment failure where outbox delivery looped forever with
// "mq: producer is not connected".
func TestRunnerConnectsProducerDuringConstruction(t *testing.T) {
	store := requireDatabase(t)
	requireRedis(t)
	runner, err := New(store)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	defer runner.Close()

	if err := runner.producer.publish(context.Background(), jobMessage{JobID: "connect-check", Attempt: 1}); err != nil {
		t.Fatalf("constructor-connected producer must publish: %v", err)
	}
}

// A Runner created for the dispatcher must reuse an already connected producer
// without requiring Work() to have been called.
func TestRunnerProducerIsConnected(t *testing.T) {
	store := requireDatabase(t)
	requireRedis(t)
	runner, err := New(store)
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	defer runner.Close()
	if err := runner.producer.publish(context.Background(), jobMessage{JobID: "connected", Attempt: 1}); err != nil {
		t.Fatalf("producer should be connected: %v", err)
	}
}
