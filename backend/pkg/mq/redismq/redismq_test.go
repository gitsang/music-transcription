package redismq

import (
	"context"
	"testing"
	"time"
)

func TestDefaultsKeepEveryEntry(t *testing.T) {
	queue := NewRedisMq()
	if queue.queueMaxLen != 0 {
		t.Fatalf("default queueMaxLen = %d, want 0 (no trimming)", queue.queueMaxLen)
	}
	if queue.consumeMode != "cluster" {
		t.Fatalf("default consume mode = %q, want cluster", queue.consumeMode)
	}
	if queue.consumeTimeout <= 0 {
		t.Fatalf("default consume timeout = %s, want positive", queue.consumeTimeout)
	}
	if queue.closeTimeout <= 0 {
		t.Fatalf("default close timeout = %s, want positive", queue.closeTimeout)
	}
	if queue.maxRetry != 3 {
		t.Fatalf("default maxRetry = %d, want 3", queue.maxRetry)
	}
	if queue.keepInterval >= queue.idleTimeout {
		t.Fatalf("keepInterval %s must be well below idleTimeout %s", queue.keepInterval, queue.idleTimeout)
	}
}

func TestZeroQueueMaxLenIsKept(t *testing.T) {
	queue := NewRedisMq(WithQueueMaxLen(1000), WithQueueMaxLen(0))
	if queue.queueMaxLen != 0 {
		t.Fatalf("queueMaxLen = %d, want 0", queue.queueMaxLen)
	}
}

func TestNegativeValuesAreIgnored(t *testing.T) {
	queue := NewRedisMq(
		WithBlockTimeout(-time.Second),
		WithKeepInterval(-time.Second),
		WithIdleTimeout(-time.Second),
		WithMaxRetry(-1),
		WithQueueMaxLen(-1),
		WithContext(nil),
		WithLogHandler(nil),
	)
	if queue.blockTimeout <= 0 || queue.keepInterval <= 0 || queue.idleTimeout <= 0 {
		t.Fatalf("negative durations must not replace defaults: %+v", queue)
	}
	if queue.maxRetry != 3 {
		t.Fatalf("maxRetry = %d, want 3", queue.maxRetry)
	}
	if queue.queueMaxLen != 0 {
		t.Fatalf("queueMaxLen = %d, want 0", queue.queueMaxLen)
	}
	if queue.ctx == nil || queue.logh == nil {
		t.Fatal("nil context / log handler must not replace the defaults")
	}
}

// TestFacadeWiresNamespaceAndStream proves the option plumbing reaches the
// producer/consumer: a message published through the facade is readable on the
// expected namespaced stream and consumable through the facade consumer.
func TestFacadeWiresNamespaceAndStream(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "facade.jobs"

	queue := NewRedisMq(
		WithRedisClient(client),
		WithNamespaces([]string{namespace}),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(10*time.Second),
		WithCloseTimeout(time.Second),
		WithMaxRetry(3),
	)

	producer := queue.Producer("facade-producer")
	if err := producer.Connect(); err != nil {
		t.Fatalf("producer Connect: %v", err)
	}
	defer producer.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := producer.Publish(ctx, newMessage(topic, `{"job_id":"facade"}`)); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// The entry must be on the namespaced stream and not trimmed away.
	stream := namespace + ":Q:" + topic
	length, err := client.XLen(ctx, stream).Result()
	if err != nil {
		t.Fatalf("XLEN %s: %v", stream, err)
	}
	if length != 1 {
		t.Fatalf("stream length = %d, want 1", length)
	}

	consumer := queue.Consumer("facade-consumer")
	if err := consumer.Connect(); err != nil {
		t.Fatalf("consumer Connect: %v", err)
	}
	defer consumer.Close()

	received := make(chan string, 1)
	if err := consumer.Subscribe(topic, nil, func(ctx context.Context, msgs ...*msgExt) error {
		for _, msg := range msgs {
			received <- string(msg.Content)
		}
		return nil
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	select {
	case content := <-received:
		if want := `{"job_id":"facade"}`; content != want {
			t.Fatalf("received %q, want %q", content, want)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("facade consumer did not receive the published message")
	}

	waitFor(t, 5*time.Second, "pending drained", func() bool {
		return pendingCount(t, client, stream, "DEFAULT_CONSUMER_GROUP") == 0
	})
}
