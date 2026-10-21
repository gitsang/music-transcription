package consumer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

const (
	minBackoff = 100 * time.Millisecond
	maxBackoff = 5 * time.Second
)

// backoff implements capped exponential backoff for consume loop errors.
type backoff struct {
	current time.Duration
}

func (b *backoff) reset() { b.current = 0 }

func (b *backoff) next() time.Duration {
	if b.current <= 0 {
		b.current = minBackoff
		return b.current
	}
	next := b.current * 2
	if next > maxBackoff {
		next = maxBackoff
	}
	b.current = next
	return b.current
}

// sleepCtx waits for d or until the context is done / the consumer is stopping.
func (c *Consumer) sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-c.stopCh:
		return false
	case <-timer.C:
		return true
	}
}

// Subscribe registers a callback for topics matching topicPattern (comma
// separated list, each entry may use Redis glob patterns) and labels.
//
// Subscriptions run in background goroutines; the returned error only
// reflects immediate validation failures.
func (c *Consumer) Subscribe(topicPattern string, labels []string, callback mq.CallbackFunc) error {
	if callback == nil {
		return errors.New("mq: nil callback")
	}
	ctx, err := c.runningContext()
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ErrClosed
	default:
	}

	selectors := utilGenTopicCombineWithLabels(splitTopics(topicPattern), labels)
	if len(selectors) == 0 {
		return errors.New("mq: empty topic pattern")
	}

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return ErrClosed
	}

	logger := c.logger.With(slog.Any("topic", topicPattern), slog.Any("labels", labels))
	c.loops.Add(1)
	go func() {
		defer c.loops.Done()
		c.consumeLoop(ctx, logger, selectors, callback)
	}()
	return nil
}

// consumeLoop resolves streams, consumes them, and retries with backoff on
// infrastructure errors until the consumer is closed.
func (c *Consumer) consumeLoop(ctx context.Context, logger *slog.Logger, selectors []string, callback mq.CallbackFunc) {
	var bo backoff
	for {
		select {
		case <-ctx.Done():
			logger.Debug("context done, exit consume loop")
			return
		case <-c.stopCh:
			logger.Debug("consumer stopping, exit consume loop")
			return
		default:
		}

		streams, err := c.matchTopics(ctx, selectors)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			logger.Error("match topics failed", slog.Any("err", err))
			if !c.sleepCtx(ctx, bo.next()) {
				return
			}
			continue
		}
		if len(streams) == 0 {
			if !c.sleepCtx(ctx, bo.next()) {
				return
			}
			continue
		}

		if c.mode == ConsumeModeCluster {
			if err := c.makeStreams(ctx, streams); err != nil {
				if ctx.Err() != nil {
					return
				}
				if !c.sleepCtx(ctx, bo.next()) {
					return
				}
				continue
			}
		}

		var consumeErr error
		switch c.mode {
		case ConsumeModeCluster:
			consumeErr = c.clusterConsume(ctx, streams, callback)
		case ConsumeModeBroadcast:
			consumeErr = c.broadcastConsume(ctx, streams, callback)
		}

		if c.stopping() || ctx.Err() != nil {
			return
		}
		if consumeErr != nil {
			delay := bo.next()
			logger.Error("consume failed", slog.Duration("backoff", delay), slog.Any("err", consumeErr))
			if !c.sleepCtx(ctx, delay) {
				return
			}
			continue
		}
		bo.reset()
	}
}
