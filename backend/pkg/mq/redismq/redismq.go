// Package redismq wires the Redis Streams producer/consumer pair behind the
// mq interfaces.
//
// Option names and defaults used by the callers are stable, except for the
// producer trim default (0 = keep all entries).
package redismq

import (
	"context"
	"log/slog"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	"github.com/gitsang/music-transcription/backend/pkg/mq/log/nop"
	rdsmqc "github.com/gitsang/music-transcription/backend/pkg/mq/redismq/consumer"
	rdsmqp "github.com/gitsang/music-transcription/backend/pkg/mq/redismq/producer"
	"github.com/go-redis/redis/v8"
)

type RedisMq struct {
	ctx         context.Context
	logh        slog.Handler
	redisClient redis.UniversalClient
	namespaces  []string

	// Consumer
	consumeMode      rdsmqc.ConsumeMode
	consumeFromWhere mq.ConsumeFromWhere
	blockTimeout     time.Duration
	keepInterval     time.Duration
	idleTimeout      time.Duration
	consumeTimeout   time.Duration
	closeTimeout     time.Duration
	maxRetry         int

	// Producer
	queueMaxLen int64
}

func defaultRedisMq() *RedisMq {
	return &RedisMq{
		ctx:              context.Background(),
		logh:             nop.New(),
		namespaces:       nil,
		consumeMode:      rdsmqc.ConsumeModeCluster,
		consumeFromWhere: mq.ConsumeFromLastOffset,
		blockTimeout:     100 * time.Millisecond,
		keepInterval:     5 * time.Second,
		idleTimeout:      30 * time.Second,
		consumeTimeout:   15 * time.Second,
		closeTimeout:     20 * time.Second,
		maxRetry:         3,
		// 0 = unlimited: a work queue must not drop entries by default.
		queueMaxLen: 0,
	}
}

var _ mq.MessageQueue = (*RedisMq)(nil)

type OptionFunc func(*RedisMq)

func WithContext(ctx context.Context) OptionFunc {
	return func(r *RedisMq) {
		if ctx != nil {
			r.ctx = ctx
		}
	}
}

func WithLogHandler(logh slog.Handler) OptionFunc {
	return func(r *RedisMq) {
		if logh != nil {
			r.logh = logh
		}
	}
}

func WithRedisClient(client redis.UniversalClient) OptionFunc {
	return func(r *RedisMq) {
		r.redisClient = client
	}
}

func WithNamespaces(namespaces []string) OptionFunc {
	return func(r *RedisMq) {
		r.namespaces = namespaces
	}
}

func WithConsumeMode(mode rdsmqc.ConsumeMode) OptionFunc {
	return func(r *RedisMq) {
		r.consumeMode = mode
	}
}

func WithConsumeFromWhere(cfw mq.ConsumeFromWhere) OptionFunc {
	return func(r *RedisMq) {
		r.consumeFromWhere = cfw
	}
}

func WithBlockTimeout(timeout time.Duration) OptionFunc {
	return func(r *RedisMq) {
		if timeout > 0 {
			r.blockTimeout = timeout
		}
	}
}

func WithKeepInterval(interval time.Duration) OptionFunc {
	return func(r *RedisMq) {
		if interval > 0 {
			r.keepInterval = interval
		}
	}
}

func WithIdleTimeout(timeout time.Duration) OptionFunc {
	return func(r *RedisMq) {
		if timeout > 0 {
			r.idleTimeout = timeout
		}
	}
}

func WithConsumeTimeout(timeout time.Duration) OptionFunc {
	return func(r *RedisMq) {
		r.consumeTimeout = timeout
	}
}

func WithCloseTimeout(timeout time.Duration) OptionFunc {
	return func(r *RedisMq) {
		r.closeTimeout = timeout
	}
}

func WithMaxRetry(maxRetry int) OptionFunc {
	return func(r *RedisMq) {
		if maxRetry >= 0 {
			r.maxRetry = maxRetry
		}
	}
}

// WithQueueMaxLen sets producer trimming. 0 (default) keeps every entry.
func WithQueueMaxLen(maxLen int64) OptionFunc {
	return func(r *RedisMq) {
		if maxLen < 0 {
			return
		}
		r.queueMaxLen = maxLen
	}
}

func NewRedisMq(optfs ...OptionFunc) *RedisMq {
	redismq := defaultRedisMq()
	for _, optf := range optfs {
		optf(redismq)
	}
	return redismq
}

func (r *RedisMq) Consumer(uniqueId string) mq.Consumer {
	consumer := rdsmqc.NewConsumer(
		rdsmqc.WithContext(r.ctx),
		rdsmqc.WithLogHandler(r.logh),
		rdsmqc.WithClient(r.redisClient),
		rdsmqc.WithNamespaces(r.namespaces),
		rdsmqc.WithUniqueId(uniqueId),
		rdsmqc.WithConsumerGroup("DEFAULT_CONSUMER_GROUP"),
		rdsmqc.WithConsumeMode(r.consumeMode),
		rdsmqc.WithConsumeFromWhere(r.consumeFromWhere),
		rdsmqc.WithBlockTimeout(r.blockTimeout),
		rdsmqc.WithKeepInterval(r.keepInterval),
		rdsmqc.WithIdleTimeout(r.idleTimeout),
		rdsmqc.WithConsumeTimeout(r.consumeTimeout),
		rdsmqc.WithCloseTimeout(r.closeTimeout),
		rdsmqc.WithMaxRetry(int64(r.maxRetry)),
	)
	return consumer
}

func (r *RedisMq) Producer(uniqueId string) mq.Producer {
	producer := rdsmqp.NewProducer(
		rdsmqp.WithContext(r.ctx),
		rdsmqp.WithLogHandler(r.logh),
		rdsmqp.WithClient(r.redisClient),
		rdsmqp.WithNamespaces(r.namespaces),
		rdsmqp.WithStreamMaxLen(r.queueMaxLen),
	)
	return producer
}
