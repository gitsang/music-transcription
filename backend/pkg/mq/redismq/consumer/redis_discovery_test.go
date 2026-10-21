package consumer

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// TestGlobTopicResolvesWithScan proves a glob subscription discovers existing
// streams, and that it never falls back to KEYS (which would block Redis).
func TestGlobTopicResolvesWithScan(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)

	// Another namespace must not leak into this pattern.
	otherNamespace := testNamespace(t, client)
	other := newTestProducer(t, client, otherNamespace)
	other.publish(t, "jobs.alpha", "[]", `{"job_id":"other"}`)

	publisher := newTestProducer(t, client, namespace)
	publisher.publish(t, "jobs.alpha", "[]", `{"job_id":"alpha"}`)
	publisher.publish(t, "jobs.beta", "[]", `{"job_id":"beta"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup("test-scan"),
		WithUniqueId("scan-worker"),
	)

	streams, err := consumer.resolveStreams(ctx, "jobs.*")
	if err != nil {
		t.Fatalf("resolveStreams: %v", err)
	}
	want := map[string]bool{
		namespace + ":Q:jobs.alpha": true,
		namespace + ":Q:jobs.beta":  true,
	}
	if len(streams) != len(want) {
		t.Fatalf("resolveStreams = %v, want %d streams", streams, len(want))
	}
	for _, stream := range streams {
		if !want[stream] {
			t.Fatalf("resolveStreams returned unexpected stream %q", stream)
		}
	}

	// A newly published stream is invisible until the cache expires, then it
	// appears without restarting the consumer.
	cached, err := consumer.resolveStreams(ctx, "jobs.*")
	if err != nil {
		t.Fatalf("resolveStreams (cached): %v", err)
	}
	if len(cached) != len(want) {
		t.Fatalf("cached resolveStreams = %v, want %d streams", cached, len(want))
	}

	publisher.publish(t, "jobs.gamma", "[]", `{"job_id":"gamma"}`)
	consumer.topicRefreshInterval = time.Millisecond
	time.Sleep(5 * time.Millisecond)
	refreshed, err := consumer.resolveStreams(ctx, "jobs.*")
	if err != nil {
		t.Fatalf("resolveStreams (refreshed): %v", err)
	}
	if len(refreshed) != len(want)+1 {
		t.Fatalf("refreshed resolveStreams = %v, want %d streams", refreshed, len(want)+1)
	}
}

// TestGlobSubscriptionDeliversAllMatchingTopics proves a glob subscription
// actually consumes every matching stream through the cluster group.
func TestGlobSubscriptionDeliversAllMatchingTopics(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	group := "test-glob"

	publisher := newTestProducer(t, client, namespace)
	publisher.publish(t, "jobs.one", "[]", `{"job_id":"one"}`)
	publisher.publish(t, "jobs.two", "[]", `{"job_id":"two"}`)

	var calls atomic.Int64
	seen := make(chan string, 8)

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("glob-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(200*time.Millisecond),
		WithIdleTimeout(5*time.Second),
		WithConsumeTimeout(10*time.Second),
		WithCloseTimeout(time.Second),
		WithTopicRefreshInterval(50*time.Millisecond),
	)
	startConsumer(t, consumer, "jobs.*", func(ctx context.Context, msgs ...*mq.MessageExt) error {
		calls.Add(1)
		for _, msg := range msgs {
			seen <- string(msg.Content)
		}
		return nil
	})

	waitFor(t, 10*time.Second, "both topics consumed", func() bool { return calls.Load() >= 2 })

	got := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case content := <-seen:
			got[content] = true
		case <-time.After(5 * time.Second):
			t.Fatalf("only received %v", got)
		}
	}
	for _, want := range []string{`{"job_id":"one"}`, `{"job_id":"two"}`} {
		if !got[want] {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
}

// TestBroadcastEverySubscriberSeesMessage proves broadcast mode uses per
// subscriber offsets instead of a consumer group.
func TestBroadcastEverySubscriberSeesMessage(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "events.broadcast"
	stream := namespace + ":Q:" + topic

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Pre-create the stream so CONSUME_FROM_LAST_OFFSET has an anchor.
	if err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: []interface{}{"topic", topic, "labels", "[]", "content", "seed"},
	}).Err(); err != nil {
		t.Fatalf("seed XADD: %v", err)
	}

	counters := make([]atomic.Int64, 2)
	consumers := make([]*Consumer, 2)
	for i := range consumers {
		idx := i
		c := NewConsumer(
			WithClient(client),
			WithNamespaces([]string{namespace}),
			WithConsumerGroup("ignored-in-broadcast"),
			WithUniqueId("broadcast-worker-"+string(rune('a'+i))),
			WithConsumeMode(ConsumeModeBroadcast),
			WithConsumeFromWhere(mq.ConsumeFromLastOffset),
			WithBlockTimeout(50*time.Millisecond),
			WithConsumeTimeout(10*time.Second),
			WithCloseTimeout(time.Second),
		)
		consumers[i] = c
		startConsumer(t, c, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
			counters[idx].Add(int64(len(msgs)))
			return nil
		})
	}

	// Let both consumers anchor on the current last id.
	time.Sleep(300 * time.Millisecond)

	newTestProducer(t, client, namespace).publish(t, topic, "[]", `{"event":"hello"}`)

	for i := range counters {
		i := i
		waitFor(t, 10*time.Second, "broadcast delivery", func() bool {
			return counters[i].Load() == 1
		})
	}

	// No group must have been created in broadcast mode.
	groups, err := client.XInfoGroups(ctx, stream).Result()
	if err != nil && err != redis.Nil {
		t.Logf("XINFO GROUPS: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("broadcast mode created %d consumer groups, want 0", len(groups))
	}
}

// TestFromFirstOffsetReplaysExistingEntries proves CONSUME_FROM_FIRST_OFFSET
// replays entries published before the subscription started.
func TestFromFirstOffsetReplaysExistingEntries(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "events.replay"

	publisher := newTestProducer(t, client, namespace)
	publisher.publish(t, topic, "[]", `{"event":"first"}`)
	publisher.publish(t, topic, "[]", `{"event":"second"}`)

	var calls atomic.Int64
	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumeMode(ConsumeModeBroadcast),
		WithConsumeFromWhere(mq.ConsumeFromFirstOffset),
		WithUniqueId("replay-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithConsumeTimeout(10*time.Second),
		WithCloseTimeout(time.Second),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		calls.Add(int64(len(msgs)))
		return nil
	})

	waitFor(t, 10*time.Second, "replay of both entries", func() bool { return calls.Load() == 2 })
}

// TestOtherNamespacesAreUntouched proves the consumer never reads or deletes
// keys outside its namespace.
func TestOtherNamespacesAreUntouched(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	otherNamespace := testNamespace(t, client)
	topic := "jobs.isolated"

	other := newTestProducer(t, client, otherNamespace)
	otherStream, otherID := other.publish(t, topic, "[]", `{"job_id":"foreign"}`)

	var calls atomic.Int64
	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup("test-isolation"),
		WithUniqueId("isolation-worker"),
		WithBlockTimeout(50*time.Millisecond),
		WithKeepInterval(100*time.Millisecond),
		WithIdleTimeout(100*time.Millisecond),
		WithConsumeTimeout(10*time.Second),
		WithCloseTimeout(time.Second),
		WithMaxRetry(1),
	)
	startConsumer(t, consumer, topic, func(ctx context.Context, msgs ...*mq.MessageExt) error {
		calls.Add(1)
		return nil
	})

	// Give the consumer time to create its own group and sweep retries/DLQ.
	time.Sleep(700 * time.Millisecond)
	if got := calls.Load(); got != 0 {
		t.Fatalf("callback ran %d times on a foreign namespace message, want 0", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entries, err := client.XRange(ctx, otherStream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRANGE foreign stream: %v", err)
	}
	if len(entries) != 1 || entries[0].ID != otherID {
		t.Fatalf("foreign stream entries = %v, want the single untouched entry %s", entries, otherID)
	}
}
