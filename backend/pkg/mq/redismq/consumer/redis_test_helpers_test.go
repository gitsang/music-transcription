package consumer

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// testRedisURLEnv names the environment variable holding a disposable Redis
// instance (database 0, no shared data). Integration tests skip when unset.
const testRedisURLEnv = "MUSIC_TEST_REDIS_URL"

// testRedis connects to the disposable Redis used by the integration tests.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()

	raw := strings.TrimSpace(os.Getenv(testRedisURLEnv))
	if raw == "" {
		t.Skipf("%s is not set; skipping Redis integration test", testRedisURLEnv)
	}
	opts, err := redis.ParseURL(raw)
	if err != nil {
		t.Fatalf("parse %s: %v", testRedisURLEnv, err)
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping %s (%s): %v", testRedisURLEnv, opts.Addr, err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// testNamespace returns a namespace unique to this test and removes only the
// keys created under it when the test ends (never FLUSHDB).
func testNamespace(t *testing.T, client *redis.Client) string {
	t.Helper()

	namespace := "mt-mqtest-" + uuid.NewString()
	t.Cleanup(func() { drainNamespace(t, client, namespace) })
	return namespace
}

// drainNamespace deletes keys matching "<namespace>:*" using SCAN only.
func drainNamespace(t *testing.T, client *redis.Client, namespace string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pattern := namespace + ":*"
	var cursor uint64
	deleted := 0
	for {
		keys, next, err := client.Scan(ctx, cursor, pattern, 256).Result()
		if err != nil {
			t.Logf("cleanup SCAN failed: %v", err)
			return
		}
		if len(keys) > 0 {
			deleted += len(keys)
			if err := client.Del(ctx, keys...).Err(); err != nil {
				t.Logf("cleanup DEL failed: %v", err)
				return
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	t.Logf("cleaned %d keys under %s", deleted, namespace)
}

// startConsumer connects a consumer, subscribes it and registers cleanup.
func startConsumer(t *testing.T, c *Consumer, topicPattern string, callback mq.CallbackFunc) {
	t.Helper()

	if err := c.Connect(); err != nil {
		t.Fatalf("consumer Connect: %v", err)
	}
	if err := c.Subscribe(topicPattern, nil, callback); err != nil {
		t.Fatalf("consumer Subscribe: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Errorf("consumer Close: %v", err)
		}
	})
}

// waitFor polls cond until it returns true or the timeout expires.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// pendingCount returns the number of pending entries of a stream group.
func pendingCount(t *testing.T, client *redis.Client, stream, group string) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	res, err := client.XPending(ctx, stream, group).Result()
	if err != nil {
		return 0
	}
	return res.Count
}

// pendingOwner returns the consumer name owning a pending entry, or "".
func pendingOwner(t *testing.T, client *redis.Client, stream, group string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pendings, err := client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream,
		Group:  group,
		Start:  "-",
		End:    "+",
		Count:  10,
	}).Result()
	if err != nil || len(pendings) == 0 {
		return ""
	}
	return pendings[0].Consumer
}

// deliveryCount returns the delivery counter of a pending entry (-1 if none).
func deliveryCount(t *testing.T, client *redis.Client, stream, group string) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pendings, err := client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream: stream,
		Group:  group,
		Start:  "-",
		End:    "+",
		Count:  10,
	}).Result()
	if err != nil || len(pendings) == 0 {
		return -1
	}
	return pendings[0].RetryCount
}

// streamLen returns XLEN, or -1 when the stream does not exist.
func streamLen(t *testing.T, client *redis.Client, stream string) int64 {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	n, err := client.XLen(ctx, stream).Result()
	if err != nil {
		return -1
	}
	return n
}

// waitStreamLen waits until stream has at least want entries.
func waitStreamLen(t *testing.T, client *redis.Client, stream string, want int64) {
	t.Helper()

	waitFor(t, 5*time.Second, "stream length", func() bool {
		return streamLen(t, client, stream) >= want
	})
}

// newTestProducer publishes into the test namespace.
func newTestProducer(t *testing.T, client *redis.Client, namespace string) *testPublisher {
	t.Helper()
	return &testPublisher{client: client, namespace: namespace}
}

// testPublisher writes messages with the same field layout the consumer reads,
// so tests can enqueue work without depending on the producer package.
type testPublisher struct {
	client    *redis.Client
	namespace string

	mu     sync.Mutex
	stream string
}

func (p *testPublisher) streamFor(topic string) string {
	return p.namespace + ":Q:" + topic
}

// publish appends one message and returns the stream name and entry id.
func (p *testPublisher) publish(t *testing.T, topic string, labels string, content string) (string, string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := p.streamFor(topic)
	id, err := p.client.XAdd(ctx, &redis.XAddArgs{
		Stream: stream,
		Values: []interface{}{
			"topic", topic,
			"labels", labels,
			"content", content,
		},
	}).Result()
	if err != nil {
		t.Fatalf("XADD %s: %v", stream, err)
	}
	return stream, id
}

// deadLetterStream mirrors the consumer naming for assertions.
func deadLetterStream(namespace, topic string) string {
	return namespace + ":DLQ:" + topic
}

// dlqEntries returns the dead letter entries of a topic.
func dlqEntries(t *testing.T, client *redis.Client, namespace, topic string) []redis.XMessage {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	entries, err := client.XRange(ctx, deadLetterStream(namespace, topic), "-", "+").Result()
	if err != nil {
		return nil
	}
	return entries
}
