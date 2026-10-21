package producer

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-redis/redis/v8"
	"github.com/google/uuid"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

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

// TestPublishDoesNotTrim proves the default producer keeps every entry, so a
// restart or a slow consumer cannot lose queued work.
func TestPublishDoesNotTrim(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)

	p := NewProducer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
		WithContext(context.Background()),
	)
	if err := p.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer p.Close()

	const total = 300
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for i := 0; i < total; i++ {
		msg := &mq.Message{Topic: "jobs.notrim", Content: []byte(`{"job_id":"x"}`)}
		if err := p.Publish(ctx, msg); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}

	stream := namespace + ":Q:jobs.notrim"
	length, err := client.XLen(ctx, stream).Result()
	if err != nil {
		t.Fatalf("XLEN: %v", err)
	}
	if length != total {
		t.Fatalf("stream length = %d, want %d (no default MAXLEN trim)", length, total)
	}
}

// TestPublishFieldLayout proves the stored fields match the consumer contract
// and that labels are encoded into the stream name.
func TestPublishFieldLayout(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)

	p := NewProducer(
		WithClient(client),
		WithNamespaces([]string{namespace}),
	)
	if err := p.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer p.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	content := `{"job_id":"job-layout","attempt":1}`
	if err := p.Publish(ctx, &mq.Message{
		Topic:   "jobs.layout",
		Labels:  []string{"b", "a"},
		Content: []byte(content),
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Labels are sorted and base64 encoded: "a,b" -> "YSxi".
	stream := namespace + ":Q:jobs.layout:YSxi"
	entries, err := client.XRange(ctx, stream, "-", "+").Result()
	if err != nil {
		t.Fatalf("XRANGE: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(entries))
	}
	values := entries[0].Values
	if got, want := values["topic"], "jobs.layout"; got != want {
		t.Errorf("topic field = %v, want %v", got, want)
	}
	if got, want := values["labels"], `["b","a"]`; got != want {
		t.Errorf("labels field = %v, want %v", got, want)
	}
	if got, want := values["content"], content; got != want {
		t.Errorf("content field = %v, want %v", got, want)
	}
}

// TestPublishRejectsAfterClose proves a closed producer refuses new work.
func TestPublishRejectsAfterClose(t *testing.T) {
	client := testRedis(t)
	namespace := testNamespace(t, client)

	p := NewProducer(WithClient(client), WithNamespaces([]string{namespace}))
	if err := p.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := p.Publish(ctx, &mq.Message{Topic: "jobs.closed", Content: []byte("x")})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}

	// Nothing must have been written.
	if _, err := client.XLen(ctx, namespace+":Q:jobs.closed").Result(); err == nil {
		length, _ := client.XLen(ctx, namespace+":Q:jobs.closed").Result()
		if length != 0 {
			t.Fatalf("stream length = %d, want 0 (closed producer must not publish)", length)
		}
	}
}
