package consumer

import (
	"context"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
)

// TestDLQTransferIsAtomic proves the dead letter transfer moves a pending entry
// in one step: exactly one DLQ entry, no duplicate on re-run, and the source
// entry acked.
func TestDLQTransferIsAtomic(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.dlqatomic"
	group := "test-dlqatomic"

	publisher := newTestProducer(t, client, namespace)
	stream, messageID := publisher.publish(t, topic, "[]", `{"job_id":"job-dlqatomic"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("XGROUP CREATE: %v", err)
	}
	if _, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: "dlq-worker",
		Streams:  []string{stream, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatalf("XREADGROUP: %v", err)
	}
	if got := pendingCount(t, client, stream, group); got != 1 {
		t.Fatalf("pending before transfer = %d, want 1", got)
	}

	dlStream := deadLetterStream(namespace, topic)
	moved, err := client.Eval(ctx, dlqTransferScript, []string{stream, dlStream},
		group, messageID, deadLetterReasonMaxRetry, "4",
	).Int64()
	if err != nil {
		t.Fatalf("dlqTransferScript: %v", err)
	}
	if moved != 1 {
		t.Fatalf("transfer result = %d, want 1", moved)
	}

	entries := dlqEntries(t, client, namespace, topic)
	if len(entries) != 1 {
		t.Fatalf("DLQ entries = %d, want 1", len(entries))
	}
	values := entries[0].Values
	if got, want := values["_entry_id"], messageID; got != want {
		t.Errorf("_entry_id = %v, want %v", got, want)
	}
	if got, want := values["_source_stream"], stream; got != want {
		t.Errorf("_source_stream = %v, want %v", got, want)
	}
	if got, want := values["_reason"], deadLetterReasonMaxRetry; got != want {
		t.Errorf("_reason = %v, want %v", got, want)
	}
	if got, want := values["_attempts"], "4"; got != want {
		t.Errorf("_attempts = %v, want %v", got, want)
	}
	if got, want := values["content"], `{"job_id":"job-dlqatomic"}`; got != want {
		t.Errorf("content = %v, want %v", got, want)
	}
	if got := pendingCount(t, client, stream, group); got != 0 {
		t.Fatalf("pending after transfer = %d, want 0 (source must be acked)", got)
	}

	// Re-running the move (crash recovery / duplicate sweep) must not create a
	// second DLQ entry: the source body is gone, so it only re-acks.
	again, err := client.Eval(ctx, dlqTransferScript, []string{stream, dlStream},
		group, messageID, deadLetterReasonMaxRetry, "4",
	).Int64()
	if err != nil {
		t.Fatalf("second dlqTransferScript: %v", err)
	}
	if again != 0 {
		t.Fatalf("second transfer result = %d, want 0 (nothing left to move)", again)
	}
	if got := len(dlqEntries(t, client, namespace, topic)); got != 1 {
		t.Fatalf("DLQ entries after re-run = %d, want 1 (no duplicates)", got)
	}
}

// TestDLQTransferKeepsAllFields proves the transfer copies every original field,
// including labels and extra producer attributes.
func TestDLQTransferKeepsAllFields(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.dlqfields"
	group := "test-dlqfields"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream := namespace + ":Q:" + topic
	messageID, err := client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: []interface{}{
			"topic", topic,
			"labels", `["x"]`,
			"content", `{"job_id":"job-fields"}`,
			"extra", "kept",
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD: %v", err)
	}
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("XGROUP CREATE: %v", err)
	}
	if _, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: "dlq-worker",
		Streams:  []string{stream, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatalf("XREADGROUP: %v", err)
	}

	dlStream := deadLetterStream(namespace, topic)
	if _, err := client.Eval(ctx, dlqTransferScript, []string{stream, dlStream},
		group, messageID, deadLetterReasonMaxRetry, "2",
	).Result(); err != nil {
		t.Fatalf("dlqTransferScript: %v", err)
	}

	entries := dlqEntries(t, client, namespace, topic)
	if len(entries) != 1 {
		t.Fatalf("DLQ entries = %d, want 1", len(entries))
	}
	values := entries[0].Values
	for field, want := range map[string]string{
		"topic":   topic,
		"labels":  `["x"]`,
		"content": `{"job_id":"job-fields"}`,
		"extra":   "kept",
	} {
		if got := values[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
}

// TestKeepScriptDoesNotBumpDeliveryCount proves renewal uses JUSTID semantics:
// a long running task is never counted as a redelivery.
func TestKeepScriptDoesNotBumpDeliveryCount(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)
	topic := "jobs.keepcount"
	group := "test-keepcount"

	publisher := newTestProducer(t, client, namespace)
	stream, messageID := publisher.publish(t, topic, "[]", `{"job_id":"job-keepcount"}`)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.XGroupCreateMkStream(ctx, stream, group, "0").Err(); err != nil {
		t.Fatalf("XGROUP CREATE: %v", err)
	}
	if _, err := client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    group,
		Consumer: "counter-worker",
		Streams:  []string{stream, ">"},
		Count:    1,
	}).Result(); err != nil {
		t.Fatalf("XREADGROUP: %v", err)
	}
	before := deliveryCount(t, client, stream, group)
	if before != 1 {
		t.Fatalf("delivery count after first read = %d, want 1", before)
	}

	consumer := NewConsumer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithConsumerGroup(group),
		WithUniqueId("counter-worker"),
	)
	for i := 0; i < 5; i++ {
		ok, err := consumer.keep(ctx, stream, messageID)
		if err != nil || !ok {
			t.Fatalf("keep #%d = (%v, %v), want (true, nil)", i, ok, err)
		}
	}
	if after := deliveryCount(t, client, stream, group); after != before {
		t.Fatalf("delivery count after 5 renewals = %d, want %d (JUSTID)", after, before)
	}
	if owner := pendingOwner(t, client, stream, group); owner != "counter-worker" {
		t.Fatalf("owner after renewal = %q, want counter-worker", owner)
	}
}
