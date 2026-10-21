package tasks

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	goredis "github.com/go-redis/redis/v8"

	"github.com/gitsang/music-transcription/backend/internal/core"
)

// requireRedis connects to the throwaway broker from MUSIC_TEST_REDIS_URL and
// resets the two stream keys this service owns. It never runs against a
// deployment broker: the test database/broker are separate opt-in variables.
func requireRedis(t *testing.T) *goredis.Client {
	t.Helper()
	raw := os.Getenv(envTestRedisURL)
	if raw == "" {
		t.Skipf("%s is not set; skipping Redis integration test", envTestRedisURL)
	}
	client := goredis.NewClient(&goredis.Options{
		Addr:     core.RedisAddr(raw),
		Password: core.RedisPassword(raw),
		DB:       core.RedisDB(raw),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("%s does not accept connections: %v", envTestRedisURL, err)
	}
	// A leftover group or stream from an aborted run would make assertions
	// ambiguous; both keys belong to this service only.
	resetStreams(t, client)
	t.Cleanup(func() {
		resetStreams(t, client)
		_ = client.Close()
	})
	return client
}

func resetStreams(t *testing.T, client *goredis.Client) {
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{streamKey(), deadLetterKey()} {
		_ = client.XGroupDestroy(ctx, key, Group).Err()
		_ = client.Del(ctx, key).Err()
	}
}

// raceResult is one handler invocation observed by the test.
type raceResult struct {
	content []byte
	err     error
}

// startTestConsumer starts the real redismq consumer through the tasks wrapper
// and returns the channel of handler invocations.
func startTestConsumer(t *testing.T, client *goredis.Client, handler func(context.Context, []byte) error) (
	*consumer, <-chan raceResult) {
	t.Helper()
	calls := make(chan raceResult, 8)
	wrapped := func(ctx context.Context, content []byte) error {
		err := handler(ctx, content)
		select {
		case calls <- raceResult{content: content, err: err}:
		default:
		}
		return err
	}
	c := newConsumer(client, "test-"+core.NewID()[:8], wrapped, 30*time.Second)
	if err := c.start(context.Background()); err != nil {
		t.Fatalf("start consumer: %v", err)
	}
	t.Cleanup(func() { _ = c.stop(5 * time.Second) })
	return c, calls
}

func waitFor(t *testing.T, timeout time.Duration, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %v", timeout, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func pendingCount(t *testing.T, client *goredis.Client) int64 {
	t.Helper()
	pending, err := client.XPending(context.Background(), streamKey(), Group).Result()
	if err != nil {
		if errors.Is(err, goredis.Nil) {
			return 0
		}
		// NOGROUP: the group was not created yet (nothing consumed).
		// NOGROUP/no-such-key: the group was never created (nothing consumed).
		if strings.Contains(err.Error(), "NOGROUP") || strings.Contains(err.Error(), "no such key") {
			return 0
		}
		t.Fatalf("xpending: %v", err)
	}
	return pending.Count
}

// TestStreamAckSemantics is the core reliability contract: a handler that
// reports success (the job reached a durable terminal state) ACKs the entry, and
// a handler that reports an infrastructure failure leaves it in the pending list
// so it is redelivered instead of being lost.
func TestStreamAckSemantics(t *testing.T) {
	t.Run("success acknowledges", func(t *testing.T) {
		client := requireRedis(t)
		producer := newProducer(client)
		if err := producer.connect(); err != nil {
			t.Fatalf("connect producer: %v", err)
		}
		t.Cleanup(func() { _ = producer.close() })

		var mu sync.Mutex
		seen := 0
		_, calls := startTestConsumer(t, client, func(context.Context, []byte) error {
			mu.Lock()
			seen++
			mu.Unlock()
			return nil
		})
		if err := producer.publish(context.Background(), jobMessage{
			JobID: "11111111-1111-4111-8111-111111111111", Attempt: 1,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case result := <-calls:
			if result.err != nil {
				t.Fatalf("handler error: %v", result.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("message was never delivered")
		}
		waitFor(t, 5*time.Second, func() error {
			if count := pendingCount(t, client); count != 0 {
				return errors.New("acknowledged message is still pending")
			}
			return nil
		})
		mu.Lock()
		defer mu.Unlock()
		if seen != 1 {
			t.Fatalf("handler invocations = %d, want 1", seen)
		}
	})

	t.Run("infrastructure failure leaves the message pending", func(t *testing.T) {
		client := requireRedis(t)
		producer := newProducer(client)
		if err := producer.connect(); err != nil {
			t.Fatalf("connect producer: %v", err)
		}
		t.Cleanup(func() { _ = producer.close() })

		failure := errors.New("infrastructure failure")
		_, calls := startTestConsumer(t, client, func(context.Context, []byte) error {
			return failure
		})
		if err := producer.publish(context.Background(), jobMessage{
			JobID: "22222222-2222-4222-8222-222222222222", Attempt: 1,
		}); err != nil {
			t.Fatalf("publish: %v", err)
		}
		select {
		case result := <-calls:
			if !errors.Is(result.err, failure) {
				t.Fatalf("handler error = %v", result.err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("message was never delivered")
		}
		// The entry must stay in the consumer group's pending list: that is what
		// makes a redelivery possible. It must not be acknowledged.
		waitFor(t, 5*time.Second, func() error {
			count := pendingCount(t, client)
			if count != 1 {
				return errors.New("a failed handler must not acknowledge the message")
			}
			return nil
		})
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if count := pendingCount(t, client); count != 1 {
				t.Fatalf("pending count changed to %d after a failure", count)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})
}

// TestStreamMalformedMessageIsDropped verifies the worker never retries a
// payload that can never decode: retrying would poison the queue forever.
func TestStreamMalformedMessageIsDropped(t *testing.T) {
	client := requireRedis(t)
	producer := newProducer(client)
	if err := producer.connect(); err != nil {
		t.Fatalf("connect producer: %v", err)
	}
	t.Cleanup(func() { _ = producer.close() })

	// The runner has no database: handleMessage must reject the payload before
	// touching the store. A panic or a returned error would fail the test.
	runner := &Runner{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, calls := startTestConsumer(t, client, func(ctx context.Context, content []byte) error {
		return runner.handleMessage(ctx, content)
	})
	if err := producer.publish(context.Background(), jobMessage{JobID: "not-a-uuid", Attempt: 1}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case result := <-calls:
		if result.err != nil {
			t.Fatalf("malformed message must be dropped, got %v", result.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("malformed message was never delivered")
	}
	waitFor(t, 5*time.Second, func() error {
		if count := pendingCount(t, client); count != 0 {
			return errors.New("malformed message was left pending")
		}
		return nil
	})
}

// TestWorkerEndToEndClaimsAcksAndPublishes exercises the full path: outbox row
// -> stream -> consumer -> claim -> pipeline subprocess -> publication -> ACK.
func TestWorkerEndToEndClaimsAcksAndPublishes(t *testing.T) {
	store := requireDatabase(t)
	client := requireRedis(t)
	job := seededJob(t, store, nil)

	runner := newTestRunner(t, store, "python3")
	runner.client = client
	realProducer := newProducer(client)
	if err := realProducer.connect(); err != nil {
		t.Fatalf("connect producer: %v", err)
	}
	runner.producer = realProducer
	t.Cleanup(func() { _ = realProducer.close() })
	runner.concurrency = 1

	if err := store.DB.Create(&core.QueueOutbox{JobID: job.ID, Attempt: 1}).Error; err != nil {
		t.Fatalf("create outbox: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	workDone := make(chan error, 1)
	go func() { workDone <- runner.Work(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-workDone:
		case <-time.After(30 * time.Second):
			t.Error("worker did not shut down")
		}
	})

	// The dispatcher normally publishes; do it once explicitly.
	if err := runner.deliverOutbox(ctx); err != nil {
		t.Fatalf("deliverOutbox: %v", err)
	}

	waitFor(t, 60*time.Second, func() error {
		current := reloadJob(t, store, job.ID)
		if current.Status != "succeeded" {
			return errors.New("job not succeeded yet: " + current.Status)
		}
		return nil
	})
	// The message must be acknowledged once the terminal state is durable.
	waitFor(t, 10*time.Second, func() error {
		if count := pendingCount(t, client); count != 0 {
			return errors.New("stream entry still pending after publication")
		}
		return nil
	})

	var tracks int64
	if err := store.DB.Model(&core.Track{}).Where("job_id = ?", job.ID).Count(&tracks).Error; err != nil {
		t.Fatalf("count tracks: %v", err)
	}
	if tracks != 1 {
		t.Fatalf("tracks = %d", tracks)
	}

	// A repeated dispatch must not re-run the terminal job.
	if err := runner.deliverOutbox(ctx); err != nil {
		t.Fatalf("second deliverOutbox: %v", err)
	}
	if err := runner.Dispatch(ctx); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if current := reloadJob(t, store, job.ID); current.Status != "succeeded" || current.Attempt != 1 {
		t.Fatalf("replay changed the job: %q attempt %d", current.Status, current.Attempt)
	}
}
