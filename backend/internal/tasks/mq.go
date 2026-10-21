package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/go-redis/redis/v8"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	rdsmqc "github.com/gitsang/music-transcription/backend/pkg/mq/redismq/consumer"
	rdsmqp "github.com/gitsang/music-transcription/backend/pkg/mq/redismq/producer"
)

// Queue identity. The Python service used Celery on the same Redis instance;
// those keys are ignored. The durable database outbox is the source of truth and
// is replayed onto this stream.
const (
	// Topic is the durable job queue topic.
	Topic = "transcription.jobs"
	// Namespace prefixes every stream owned by this service.
	Namespace = "music-transcription"
	// Group is the shared consumer group for cluster-mode delivery.
	Group = "transcription-workers"
)

// The stream names below mirror pkg/mq/redismq naming (namespace + queue kind +
// topic) so logs and operator tooling address the same keys the consumers use.
func streamKey() string     { return strings.Join([]string{Namespace, "Q", Topic}, ":") }
func deadLetterKey() string { return strings.Join([]string{Namespace, "DLQ", Topic}, ":") }

// jobMessage is the only payload allowed on the queue: no paths, no commands.
type jobMessage struct {
	JobID   string `json:"job_id"`
	Attempt int    `json:"attempt"`
}

// producer publishes job messages on the shared stream. It is a thin adapter
// over pkg/mq/redismq/producer so the queue layout, label handling and (absent)
// trimming behaviour stay identical to the vendored implementation.
type producer struct {
	inner mq.Producer
}

func newProducer(client redis.UniversalClient) *producer {
	inner := rdsmqp.NewProducer(
		rdsmqp.WithClient(client),
		rdsmqp.WithNamespaces([]string{Namespace}),
		// Never trim: a work queue must not drop entries that were not yet
		// consumed. The database outbox would replay them, but silently losing
		// broker state makes backlogs invisible.
		rdsmqp.WithStreamMaxLen(0),
		rdsmqp.WithOperationTimeout(10*time.Second),
	)
	return &producer{inner: inner}
}

func (p *producer) connect() error { return p.inner.Connect() }

// publish writes one job message. It never trims the stream; callers treat an
// error as an infrastructure failure and keep the outbox row undelivered.
func (p *producer) publish(ctx context.Context, msg jobMessage) error {
	content, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return p.inner.Publish(ctx, &mq.Message{Topic: Topic, Content: content})
}

func (p *producer) close() error { return p.inner.Close() }

// consumer delivers job messages in cluster mode. All the long-running-task
// semantics (consume deadline handed to the callback, keep-alive that only
// renews while this consumer still owns the message, Lua-based dead letter
// transfer) come from the vendored implementation instead of being reimplemented
// here.
type consumer struct {
	inner   mq.Consumer
	handler func(context.Context, []byte) error
	logger  *slog.Logger
}

func newConsumer(client redis.UniversalClient, uniqueID string,
	handler func(context.Context, []byte) error, consumeTimeout time.Duration) *consumer {
	if consumeTimeout <= 0 {
		consumeTimeout = time.Hour
	}
	inner := rdsmqc.NewConsumer(
		rdsmqc.WithClient(client),
		rdsmqc.WithUniqueId(uniqueID),
		rdsmqc.WithNamespaces([]string{Namespace}),
		rdsmqc.WithConsumerGroup(Group),
		rdsmqc.WithConsumeMode(rdsmqc.ConsumeModeCluster),
		// The callback context must outlive a whole pipeline run, otherwise a
		// slow job would be cancelled mid-flight.
		rdsmqc.WithConsumeTimeout(consumeTimeout),
		rdsmqc.WithIdleTimeout(30*time.Second),
		rdsmqc.WithKeepInterval(5*time.Second),
		rdsmqc.WithBlockTimeout(100*time.Millisecond),
		rdsmqc.WithMaxRetry(3),
		rdsmqc.WithCloseTimeout(10*time.Second),
	)
	return &consumer{
		inner:   inner,
		handler: handler,
		logger:  slog.Default().With(slog.String("consumer", uniqueID)),
	}
}

// start connects and begins consuming. The callback returns an error only for
// infrastructure failures (which leave the message pending for redelivery); a
// durably resolved job - including a cancelled or failed one - returns nil and is
// acknowledged. Messages whose payload cannot be decoded are dropped by the
// handler, since retrying cannot fix them.
func (c *consumer) start(ctx context.Context) error {
	if err := c.inner.Connect(); err != nil {
		return fmt.Errorf("tasks: connect consumer: %w", err)
	}
	return c.inner.Subscribe(Topic, nil, func(ctx context.Context, messages ...*mq.MessageExt) error {
		for _, message := range messages {
			if message == nil {
				continue
			}
			if err := c.handler(ctx, message.Content); err != nil {
				// Stops the batch: the message stays in the pending list and is
				// retried after the idle timeout or by another worker.
				return err
			}
		}
		return nil
	})
}

// stop closes the consumer. The vendored consumer owns its close timeout
// (configured at construction), so the argument is only kept for call-site
// symmetry with the shutdown code.
func (c *consumer) stop(_ time.Duration) error { return c.inner.Close() }
