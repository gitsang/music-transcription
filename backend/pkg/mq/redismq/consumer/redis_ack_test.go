package consumer

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// TestClusterAckOnCallbackSuccess proves the happy path: a delivered message is
// acked and nothing stays pending.
func TestClusterAckOnCallbackSuccess(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.ack"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-1"}`)

	var calls atomic.Int64

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup("test-ack"),
		WithUniqueId("ack-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(50*time.Millisecond),
		WithIdleTimeout(2*time.Second),
		WithConsumeTimeout(5*time.Second),
		WithCloseTimeout(time.Second),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		calls.Add(1)
		if len(msgs) != 1 {
			t.Errorf("callback got %d messages, want 1", len(msgs))
			return nil
		}
		if got, want := msgs[0].Topic, topic; got != want {
			t.Errorf("message topic = %q, want %q", got, want)
		}
		if got, want := string(msgs[0].Content), `{"job_id":"job-1"}`; got != want {
			t.Errorf("message content = %q, want %q", got, want)
		}
		return nil
	})

	waitFor(t, 5*time.Second, "callback", func() bool { return calls.Load() == 1 })
	waitFor(t, 5*time.Second, "pending drained", func() bool {
		return pendingCount(t, client, stream, "test-ack") == 0
	})
}

// TestClusterRetryOnCallbackError proves a failed callback leaves the message
// pending so the retry path can redeliver it.
func TestClusterRetryOnCallbackError(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.retry"
	group := "test-retry"

	publisher := newTestProducer(t, client, namespace)
	stream, _ := publisher.publish(t, topic, "[]", `{"job_id":"job-retry"}`)

	var attempts atomic.Int64
	failUntil := int64(2)

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("retry-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(200*time.Millisecond),
		WithIdleTimeout(200*time.Millisecond),
		WithConsumeTimeout(5*time.Second),
		WithCloseTimeout(time.Second),
		WithMaxRetry(5),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		if attempts.Add(1) <= failUntil {
			return errors.New("transient failure")
		}
		return nil
	})

	waitFor(t, 15*time.Second, "retry delivered then acked", func() bool {
		return attempts.Load() > failUntil && pendingCount(t, client, stream, group) == 0
	})
	if got := attempts.Load(); got < failUntil+1 {
		t.Fatalf("attempts = %d, want at least %d", got, failUntil+1)
	}
}

// TestClusterDeadLetterAfterMaxRetry proves exhausted messages are moved into
// the DLQ stream and removed from the pending list.
func TestClusterDeadLetterAfterMaxRetry(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.dlq"
	group := "test-dlq"

	publisher := newTestProducer(t, client, namespace)
	stream, messageID := publisher.publish(t, topic, "[]", `{"job_id":"job-dlq"}`)

	var attempts atomic.Int64

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("dlq-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(100*time.Millisecond),
		WithConsumeTimeout(5*time.Second),
		WithCloseTimeout(time.Second),
		WithMaxRetry(1),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		attempts.Add(1)
		return errors.New("permanent failure")
	})

	waitFor(t, 15*time.Second, "dead letter entry", func() bool {
		return len(dlqEntries(t, client, namespace, topic)) > 0
	})

	entries := dlqEntries(t, client, namespace, topic)
	values := entries[0].Values
	if got, want := values["_entry_id"], messageID; got != want {
		t.Errorf("DLQ _entry_id = %v, want %v", got, want)
	}
	if got, want := values["_source_stream"], stream; got != want {
		t.Errorf("DLQ _source_stream = %v, want %v", got, want)
	}
	if got, want := values["_reason"], deadLetterReasonMaxRetry; got != want {
		t.Errorf("DLQ _reason = %v, want %v", got, want)
	}
	if got := values["content"]; got != `{"job_id":"job-dlq"}` {
		t.Errorf("DLQ content = %v", got)
	}

	// The transfer is atomic: the source entry must be acked, not pending.
	waitFor(t, 5*time.Second, "source acked after DLQ transfer", func() bool {
		return pendingCount(t, client, stream, group) == 0
	})
	if got := attempts.Load(); got < 2 {
		t.Errorf("attempts = %d, want at least 2 (initial + retry)", got)
	}
}
