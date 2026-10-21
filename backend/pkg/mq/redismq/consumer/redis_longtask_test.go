package consumer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// TestLongTaskRenewKeepsOwnership proves a callback running far longer than the
// idle timeout keeps ownership and is neither redelivered nor stolen.
func TestLongTaskRenewKeepsOwnership(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.long"
	group := "test-long"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-long"}`)

	var calls atomic.Int64
	release := make(chan struct{})

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("long-worker"),
		WithBlockTimeout(50*time.Millisecond),
		// Renew far faster than the idle timeout: without renewal the message
		// would be considered idle and reclaimed by this same consumer.
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(300*time.Millisecond),
		WithConsumeTimeout(30*time.Second),
		WithCloseTimeout(5*time.Second),
		WithMaxRetry(5),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		calls.Add(1)
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})

	waitFor(t, 5*time.Second, "long callback started", func() bool { return calls.Load() == 1 })

	// Hold the callback for several idle timeouts.
	time.Sleep(1200 * time.Millisecond)

	if got := calls.Load(); got != 1 {
		t.Fatalf("callback ran %d times, want 1 (renewal must prevent redelivery)", got)
	}
	if owner := pendingOwner(t, client, stream, group); owner != "long-worker" {
		t.Fatalf("pending owner = %q, want %q", owner, "long-worker")
	}
	if count := deliveryCount(t, client, stream, group); count != 1 {
		t.Fatalf("delivery count = %d, want 1 (renewal must not bump the counter)", count)
	}

	close(release)
	waitFor(t, 10*time.Second, "long callback acked", func() bool {
		return pendingCount(t, client, stream, group) == 0
	})
	if got := calls.Load(); got != 1 {
		t.Fatalf("callback ran %d times in total, want 1", got)
	}
}

// TestCancelStopsRunningCallback proves ctx cancellation aborts a running
// callback and leaves the message pending for another consumer.
func TestCancelStopsRunningCallback(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.cancel"
	group := "test-cancel"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-cancel"}`)

	started := make(chan struct{})
	cancelled := make(chan struct{})

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("cancel-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(time.Hour),
		// Small close timeout so the test does not wait the default 20s.
		WithCloseTimeout(300*time.Millisecond),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		close(started)
		<-ctx.Done()
		close(cancelled)
		return ctx.Err()
	})

	waitFor(t, 5*time.Second, "callback started", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	if err := consumer.Close(); err != nil {
		t.Fatalf("consumer Close: %v", err)
	}

	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not cancel the running callback context")
	}

	// The work did not finish, so the entry must still be pending (not acked).
	if got := pendingCount(t, client, stream, group); got != 1 {
		t.Fatalf("pending after cancel = %d, want 1", got)
	}
}

// TestCloseStopsPullingAndAcksInflight proves Close stops new deliveries,
// waits for the in-flight callback to finish and keeps its ack.
func TestCloseStopsPullingAndAcksInflight(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.close"
	group := "test-close"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-close-1"}`)

	var calls atomic.Int64
	started := make(chan struct{})

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("close-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(30*time.Second),
		WithCloseTimeout(10*time.Second),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		if calls.Add(1) == 1 {
			close(started)
			// Simulate work that outlives the Close call itself.
			time.Sleep(600 * time.Millisecond)
		}
		return nil
	})

	waitFor(t, 5*time.Second, "first callback started", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	// Queue more work while the callback runs; it must NOT be picked up.
	publisher.publish(t, topic, "[]", `{"job_id":"job-close-2"}`)

	closed := make(chan error, 1)
	go func() { closed <- consumer.Close() }()

	// While Close is waiting, the first callback must still complete.
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("consumer Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return within the close timeout")
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("callback ran %d times, want 1 (Close must not drain the backlog)", got)
	}
	// The drained callback acked its own entry; the queued one is still there.
	if got := pendingCount(t, client, stream, group); got != 0 {
		t.Fatalf("pending after Close = %d, want 0 (finished work is acked)", got)
	}
	if got := streamLen(t, client, stream); got != 2 {
		t.Fatalf("stream length = %d, want 2 (producer must not trim)", got)
	}

	// Confirm the queued entry was never delivered: a fresh consumer in the same
	// group must receive it next.
	var secondCalls atomic.Int64
	var secondContent atomic.Value
	second := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("close-worker-2"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(10*time.Second),
		WithCloseTimeout(time.Second),
	)
	startConsumer(t, second, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		secondCalls.Add(1)
		if len(msgs) == 1 {
			secondContent.Store(string(msgs[0].Content))
		}
		return nil
	})

	waitFor(t, 10*time.Second, "queued entry delivered once", func() bool {
		return secondCalls.Load() == 1
	})
	if got, want := secondContent.Load(), `{"job_id":"job-close-2"}`; got != want {
		t.Fatalf("second consumer content = %v, want the queued job-close-2", got)
	}
	if got := pendingCount(t, client, stream, group); got != 0 {
		t.Fatalf("pending after second consumer = %d, want 0", got)
	}
}

// TestCloseTimeoutCancelsInflight proves a close timeout smaller than the
// callback duration really cancels it instead of returning silently.
func TestCloseTimeoutCancelsInflight(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.closetimeout"
	group := "test-closetimeout"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-ct"}`)

	started := make(chan struct{})
	interrupted := make(chan error, 1)

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("close-timeout-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(time.Hour),
		WithCloseTimeout(200*time.Millisecond),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		close(started)
		<-ctx.Done()
		interrupted <- ctx.Err()
		return ctx.Err()
	})

	waitFor(t, 5*time.Second, "callback started", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	begin := time.Now()
	if err := consumer.Close(); err != nil {
		t.Fatalf("consumer Close: %v", err)
	}
	elapsed := time.Since(begin)
	if elapsed > 3*time.Second {
		t.Fatalf("Close took %s, want it bounded by the close timeout", elapsed)
	}

	select {
	case err := <-interrupted:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("callback ctx error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("callback was not cancelled by the close timeout")
	}

	if got := pendingCount(t, client, stream, group); got != 1 {
		t.Fatalf("pending after forced close = %d, want 1", got)
	}
}

// TestOwnershipLossCancelsCallback proves that when another consumer claims the
// pending message, the original callback is cancelled and never acks.
func TestOwnershipLossCancelsCallback(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.ownerloss"
	group := "test-ownerloss"

	publisher := newTestProducer(t, client, namespace)
	stream, messageID := publisher.publish(t, topic, "[]", `{"job_id":"job-owner"}`)

	started := make(chan struct{})
	interrupted := make(chan error, 1)

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("original-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(time.Hour),
		WithCloseTimeout(5*time.Second),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		close(started)
		<-ctx.Done()
		interrupted <- ctx.Err()
		// A well-behaved callback reports the failure; the consumer must not
		// ack it anyway.
		return errors.New("aborted by ownership loss")
	})

	waitFor(t, 5*time.Second, "callback started", func() bool {
		select {
		case <-started:
			return true
		default:
			return false
		}
	})

	// Another consumer steals the message (min-idle 0 forces the claim).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claimed, err := client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   stream,
		Group:    group,
		Consumer: "thief-worker",
		MinIdle:  0,
		Messages: []string{messageID},
	}).Result()
	if err != nil {
		t.Fatalf("XCLAIM steal: %v", err)
	}
	if len(claimed) != 1 {
		t.Fatalf("steal claimed %d entries, want 1", len(claimed))
	}

	select {
	case <-interrupted:
	case <-time.After(5 * time.Second):
		t.Fatal("ownership loss did not cancel the callback")
	}

	waitFor(t, 5*time.Second, "owner switched", func() bool {
		return pendingOwner(t, client, stream, group) == "thief-worker"
	})
	// The original consumer's keep must not steal ownership back.
	time.Sleep(500 * time.Millisecond)
	if owner := pendingOwner(t, client, stream, group); owner != "thief-worker" {
		t.Fatalf("pending owner = %q, want thief-worker (no XCLAIM steal-back)", owner)
	}
	if got := pendingCount(t, client, stream, group); got != 1 {
		t.Fatalf("pending after ownership loss = %d, want 1 (must not be acked)", got)
	}
}

// TestKeepErrorCancelsCallback proves that repeated renewal infrastructure
// errors cancel the callback context instead of letting it ack blindly.
func TestKeepErrorCancelsCallback(t *testing.T) {
	// An unreachable address makes every renewal attempt fail with a
	// connection error (not an ownership error).
	broken := redis.NewClient(&redis.Options{
		Addr:        "127.0.0.1:1",
		DialTimeout: 30 * time.Millisecond,
		MaxRetries:  -1,
	})
	defer broken.Close()

	consumer := NewConsumer(
		WithClient(broken),
		WithNamespaces([]string{"mt-mqtest-unused"}),
		WithConsumerGroup("test-keepfail"),
		WithUniqueId("keepfail-worker"),
		WithKeepInterval(20*time.Millisecond),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newOwnerGuard(cancel)

	done := make(chan struct{})
	go func() {
		defer close(done)
		consumer.keeping(ctx, guard, "mt-mqtest-unused:Q:jobs.keepfail", "1-0")
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("keeping did not give up after repeated renewal failures")
	}
	if !guard.Lost() {
		t.Fatal("repeated keep errors must mark ownership as lost and cancel the callback")
	}
	if ctx.Err() == nil {
		t.Fatal("repeated keep errors must cancel the callback context")
	}
}

// TestKeepDetectsForeignOwner proves the renewal script refuses to steal back a
// message the group already handed to another consumer.
func TestKeepDetectsForeignOwner(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.foreignowner"
	group := "test-foreignowner"

	publisher := newTestProducer(t, client, namespace)
	stream, messageID := publisher.publish(t, topic, "[]", `{"job_id":"job-foreign"}`)

	// Create the group and deliver the message to another consumer.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("XGROUP CREATE: %v", err)
	}
	if _, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: "peer-worker",
		Streams:  []string{stream, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatalf("XREADGROUP: %v", err)
	}

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("other-worker"),
	)

	ok, err := consumer.keep(ctx, stream, messageID)
	if ok {
		t.Fatal("keep must not renew a message owned by another consumer")
	}
	if !errors.Is(err, ErrOwnershipLost) {
		t.Fatalf("keep error = %v, want ErrOwnershipLost", err)
	}
	if owner := pendingOwner(t, client, stream, group); owner != "peer-worker" {
		t.Fatalf("pending owner = %q, want peer-worker (no steal)", owner)
	}
}
