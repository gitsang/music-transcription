package redismq

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

// testRedisURLEnv names the environment variable holding a disposable Redis
// instance. Facade tests skip when it is unset.
const testRedisURLEnv = "MUSIC_TEST_REDIS_URL"

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
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pattern := namespace + ":*"
		var cursor uint64
		for {
			keys, next, err := client.Scan(ctx, cursor, pattern, 256).Result()
			if err != nil {
				t.Logf("cleanup SCAN failed: %v", err)
				return
			}
			if len(keys) > 0 {
				if err := client.Del(ctx, keys...).Err(); err != nil {
					t.Logf("cleanup DEL failed: %v", err)
					return
				}
			}
			cursor = next
			if cursor == 0 {
				return
			}
		}
	})
	return namespace
}

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

func newMessage(topic, content string) *mq.Message {
	return &mq.Message{Topic: topic, Content: []byte(content)}
}

// msgExt is an alias so the facade test reads like the public API.
type msgExt = mq.MessageExt
