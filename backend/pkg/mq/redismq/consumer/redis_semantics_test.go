package consumer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// TestCallbackContextCarriesConsumeTimeout proves the callback gets a context
// with the configured consume deadline (upstream handed it the bare running
// context, so a stuck callback held the read loop forever).
func TestCallbackContextCarriesConsumeTimeout(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.deadline"
	group := "test-deadline"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-deadline"}`)

	deadlineSeen := make(chan time.Duration, 1)
	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("deadline-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(1500*time.Millisecond),
		WithCloseTimeout(time.Second),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		deadline, ok := ctx.Deadline()
		if !ok {
			deadlineSeen <- 0
			return errors.New("no deadline")
		}
		deadlineSeen <- time.Until(deadline)
		return nil
	})

	select {
	case remaining := <-deadlineSeen:
		if remaining <= 0 {
			t.Fatalf("callback context has no positive deadline (remaining %s)", remaining)
		}
		if remaining > 1500*time.Millisecond {
			t.Fatalf("deadline remaining %s exceeds the consume timeout", remaining)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("callback was never invoked")
	}

	waitFor(t, 5*time.Second, "acked", func() bool {
		return pendingCount(t, client, stream, group) == 0
	})
}

// TestZeroConsumeTimeoutDisablesDeadline proves a non-positive consume timeout
// means "no deadline" instead of "already expired".
func TestZeroConsumeTimeoutDisablesDeadline(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.nodeadline"
	group := "test-nodeadline"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-nodeadline"}`)

	result := make(chan bool, 1)
	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("nodeadline-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(0),
		WithCloseTimeout(time.Second),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		_, hasDeadline := ctx.Deadline()
		result <- hasDeadline
		return nil
	})

	select {
	case hasDeadline := <-result:
		if hasDeadline {
			t.Fatal("consume timeout 0 must not impose a deadline")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("callback was never invoked")
	}

	waitFor(t, 5*time.Second, "acked", func() bool {
		return pendingCount(t, client, stream, group) == 0
	})
}

// TestCallbackPanicLeavesMessagePending proves a panicking callback is
// recovered and the message is not acked.
func TestCallbackPanicLeavesMessagePending(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.panic"
	group := "test-panic"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-panic"}`)

	var calls atomic.Int64
	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("panic-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(10*time.Second),
		WithCloseTimeout(time.Second),
		// No retries and a long idle timeout: the entry must stay pending.
		WithMaxRetry(0),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		calls.Add(1)
		panic("boom")
	})

	waitFor(t, 10*time.Second, "panicking callback", func() bool { return calls.Load() == 1 })
	time.Sleep(300 * time.Millisecond)
	if got := pendingCount(t, client, stream, group); got != 1 {
		t.Fatalf("pending after panic = %d, want 1", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("panicking callback ran %d times, want 1 (no retry within idle timeout)", got)
	}
}

// TestLifecycleGuards covers the Connect/Subscribe/Close state machine.
func TestLifecycleGuards(t *testing.T) {
	client := testRedis(t)

	t.Run("subscribe before connect", func(t *testing.T) {
		c := NewConsumer(WithClient(client))
		err := c.Subscribe("jobs.x", nil, func(context.Context, ...*mq.MessageExt) error { return nil })
		if !errors.Is(err, ErrNotConnected) {
			t.Fatalf("Subscribe before Connect = %v, want ErrNotConnected", err)
		}
	})

	t.Run("nil callback", func(t *testing.T) {
		c := NewConsumer(WithClient(client))
		if err := c.Connect(); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer c.Close()
		if err := c.Subscribe("jobs.x", nil, nil); err == nil {
			t.Fatal("Subscribe with a nil callback must fail")
		}
	})

	t.Run("empty topic", func(t *testing.T) {
		c := NewConsumer(WithClient(client))
		if err := c.Connect(); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		defer c.Close()
		if err := c.Subscribe("  ", nil, func(context.Context, ...*mq.MessageExt) error { return nil }); err == nil {
			t.Fatal("Subscribe with an empty topic pattern must fail")
		}
	})

	t.Run("close is idempotent", func(t *testing.T) {
		c := NewConsumer(WithClient(client))
		if err := c.Connect(); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		if err := c.Connect(); err != nil {
			t.Fatalf("second Connect must be a no-op, got %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("second Close must be a no-op, got %v", err)
		}
		if err := c.Connect(); !errors.Is(err, ErrClosed) {
			t.Fatalf("Connect after Close = %v, want ErrClosed", err)
		}
		if err := c.Subscribe("jobs.x", nil, func(context.Context, ...*mq.MessageExt) error { return nil }); !errors.Is(err, ErrClosed) {
			t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
		}
		if got := c.Inflight(); got != 0 {
			t.Fatalf("Inflight after Close = %d, want 0", got)
		}
	})

	t.Run("connect without client", func(t *testing.T) {
		c := NewConsumer()
		if err := c.Connect(); err == nil {
			t.Fatal("Connect without a Redis client must fail")
		}
	})
}
