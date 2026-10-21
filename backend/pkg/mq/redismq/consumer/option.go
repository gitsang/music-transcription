// Package consumer implements a Redis Streams consumer with cluster and
// broadcast modes, retry, a dead letter queue and keep-alive.
//
// The long-running-task behaviour has these properties:
//
//   - the cluster callback now receives a context carrying the consume
//     timeout instead of the bare running context;
//   - keep-alive failures are propagated to the callback context, so a
//     consumer that lost ownership stops working instead of acking;
//   - keep-alive renews ownership atomically (Lua owner comparison) instead
//     of unconditionally XCLAIM-ing a message another consumer already owns;
//   - Close stops pulling new messages, waits for in-flight callbacks and
//     enforces the configured close timeout with a real cancellation.
package consumer

import (
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	"github.com/google/uuid"
)

type ConsumeMode string

const (
	ConsumeModeCluster   ConsumeMode = "cluster"
	ConsumeModeBroadcast ConsumeMode = "broadcast"
)

type consumerOptions struct {
	// Consume mode
	// - cluster
	// - broadcast
	mode ConsumeMode

	consumeFromWhere mq.ConsumeFromWhere

	// Consumer unique id
	uniqueId string

	// The consumer group name.
	group string

	// The consumer namespaces.
	namespaces []string

	// The time to wait for each queue to be read.
	blockTimeout time.Duration

	// The interval between sending message keep signals.
	keepInterval time.Duration

	// The maximum time a message can stay idle.
	// Idle time is the elapsed since the last keep signal received.
	// When exceeds the timeout, the message will be redelivered.
	idleTimeout time.Duration

	// The maximum time a message can be consumed.
	// Consume time is the duration since the message was delivered.
	// When exceeds the timeout, the message will be redelivered.
	//
	// A non-positive value disables the deadline.
	consumeTimeout time.Duration

	// The timeout for stopping the `Subscribe` process.
	//
	// - `closeTimeout = 0` means there is no timeout, and the process will
	//   continue until all messages are consumed before exiting.
	// - `closeTimeout < 0` means no waiting, and all consuming tasks will
	//   be terminated immediately.
	closeTimeout time.Duration

	// The maximum times a message can be retried.
	// When retries exceed, the message will go into a dead letter queue.
	maxRetry int64

	// How often a glob topic pattern is re-resolved with SCAN.
	topicRefreshInterval time.Duration
}

func defaultConsumerOptions() *consumerOptions {
	return &consumerOptions{
		mode:                 ConsumeModeCluster,
		consumeFromWhere:     mq.ConsumeFromLastOffset,
		uniqueId:             uuid.NewString(),
		group:                "DEFAULT_CONSUMER_GROUP",
		namespaces:           nil,
		blockTimeout:         100 * time.Millisecond,
		keepInterval:         5 * time.Second,
		idleTimeout:          30 * time.Second,
		consumeTimeout:       15 * time.Second,
		closeTimeout:         20 * time.Second,
		maxRetry:             3,
		topicRefreshInterval: 5 * time.Second,
	}
}

type Option func(*Consumer)

func WithConsumeFromWhere(cfw mq.ConsumeFromWhere) Option {
	return func(c *Consumer) {
		c.consumeFromWhere = cfw
	}
}

func WithConsumeMode(mode ConsumeMode) Option {
	return func(c *Consumer) {
		switch mode {
		case ConsumeModeBroadcast:
			c.mode = ConsumeModeBroadcast
		default:
			c.mode = ConsumeModeCluster
		}
	}
}

func WithUniqueId(id string) Option {
	return func(c *Consumer) {
		if id == "" {
			return
		}
		c.uniqueId = id
	}
}

func WithConsumerGroup(group string) Option {
	return func(c *Consumer) {
		if group == "" {
			return
		}
		c.group = group
	}
}

func WithNamespaces(namespaces []string) Option {
	return func(c *Consumer) {
		c.namespaces = namespaces
	}
}

func WithBlockTimeout(timeout time.Duration) Option {
	return func(c *Consumer) {
		if timeout > 0 {
			c.blockTimeout = timeout
		}
	}
}

func WithKeepInterval(interval time.Duration) Option {
	return func(c *Consumer) {
		if interval > 0 {
			c.keepInterval = interval
		}
	}
}

// WithIdleTimeout set max idle timeout.
// When exceeds the timeout, the message will be redelivered.
// Consumer should keepalive to refresh idle timeout to avoid message loss.
func WithIdleTimeout(timeout time.Duration) Option {
	return func(c *Consumer) {
		if timeout > 0 {
			c.idleTimeout = timeout
		}
	}
}

// WithConsumeTimeout sets the deadline handed to the callback. A value <= 0
// disables the deadline (the callback still observes cancellation).
func WithConsumeTimeout(timeout time.Duration) Option {
	return func(c *Consumer) {
		c.consumeTimeout = timeout
	}
}

func WithCloseTimeout(timeout time.Duration) Option {
	return func(c *Consumer) {
		c.closeTimeout = timeout
	}
}

// WithMaxRetry set max retry count.
// Count < 0 will not change but keep the default retry count.
// Count == 0 means message will try once and not retry.
// Count > 0 means message will try once and then retry the count times.
func WithMaxRetry(count int64) Option {
	return func(c *Consumer) {
		if count >= 0 {
			c.maxRetry = count
		}
	}
}

// WithTopicRefreshInterval sets how often glob patterns are re-resolved
// against Redis. Exact (non-glob) topics never trigger a scan.
func WithTopicRefreshInterval(interval time.Duration) Option {
	return func(c *Consumer) {
		if interval > 0 {
			c.topicRefreshInterval = interval
		}
	}
}
