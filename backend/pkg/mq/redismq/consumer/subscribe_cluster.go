package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	"github.com/gitsang/music-transcription/backend/pkg/mq/redismq/model"
	"github.com/go-redis/redis/v8"
)

// keepScript renews ownership of a pending message only while this consumer
// still owns it.
//
// Upstream used an unconditional XCLAIM with MinIdle=0, which could steal a
// message the group had already handed to another consumer (e.g. after this
// consumer stalled past idleTimeout). Two workers would then process the same
// message. The script checks ownership first and only then performs XCLAIM
// (which resets the idle timer, keeping the message alive for this consumer).
//
// KEYS[1] = stream, ARGV[1] = group, ARGV[2] = consumer, ARGV[3] = message id
// Returns 1 if renewed, 0 if this consumer no longer owns the message.
const keepScript = `
local pending = redis.call('XPENDING', KEYS[1], ARGV[1], ARGV[3], ARGV[3], 1, ARGV[2])
if (not pending) or (#pending == 0) then return 0 end
local claimed = redis.call('XCLAIM', KEYS[1], ARGV[1], ARGV[2], 0, ARGV[3], 'JUSTID')
if (not claimed) or (#claimed == 0) then return 0 end
return 1
`

// dlqTransferScript moves a pending message to the dead letter stream and acks
// it, atomically and idempotently.
//
// Upstream issued XADD and XACK as separate commands: a crash in between
// re-delivered the message and produced a duplicate DLQ entry, while a failed
// XACK left it pending forever. Doing both in one script removes the double
// write. The script also requires the entry to still be pending for this
// group: XACK does not delete the stream entry, so a retried transfer (crash
// after the script succeeded, or a duplicate sweep) must not append a second
// DLQ copy.
//
// KEYS[1] = source stream, KEYS[2] = dead letter stream
// ARGV[1] = group, ARGV[2] = message id, ARGV[3] = reason, ARGV[4] = attempts
// Returns 1 if moved, 0 if there was nothing left to move (already acked or
// the body disappeared).
const dlqTransferScript = `
local id = ARGV[2]
local pending = redis.call('XPENDING', KEYS[1], ARGV[1], id, id, 1)
if (not pending) or (#pending == 0) then return 0 end
local entry = redis.call('XRANGE', KEYS[1], id, id)
if (not entry) or (#entry == 0) then
  redis.call('XACK', KEYS[1], ARGV[1], id)
  return 0
end
local fields = entry[1][2]
local out = {'_entry_id', id, '_source_stream', KEYS[1], '_reason', ARGV[3], '_attempts', ARGV[4]}
for i = 1, #fields do
  out[#out + 1] = fields[i]
end
redis.call('XADD', KEYS[2], '*', unpack(out))
redis.call('XACK', KEYS[1], ARGV[1], id)
return 1
`

const (
	// keepErrorRetries bounds consecutive renewal infrastructure errors before
	// the callback context is cancelled.
	keepErrorRetries = 3

	// deadLetterReason prefix stored in the DLQ entry.
	deadLetterReasonMaxRetry = "max_retry_exceeded"
)

// ownerGuard tracks ownership of one in-flight message and cancels the
// callback context when ownership is lost.
type ownerGuard struct {
	lost   atomic.Bool
	cancel context.CancelFunc
}

func newOwnerGuard(cancel context.CancelFunc) *ownerGuard {
	return &ownerGuard{cancel: cancel}
}

func (g *ownerGuard) fail() {
	g.lost.Store(true)
	g.cancel()
}

func (g *ownerGuard) Lost() bool { return g.lost.Load() }

// keep renews ownership of one pending message.
// It returns false when this consumer is no longer the owner.
func (c *Consumer) keep(ctx context.Context, stream string, messageID string) (bool, error) {
	logger := c.logger.With(slog.String("stream", stream), slog.String("messageID", messageID))
	res, err := c.client.Eval(ctx, keepScript, []string{stream},
		c.group, c.uniqueId, messageID,
	).Int64()
	if err != nil {
		logger.Error("XCLAIM keep failed", slog.Any("err", err))
		return false, err
	}
	if res == 1 {
		return true, nil
	}
	logger.Warn("message ownership lost")
	return false, ErrOwnershipLost
}

// keeping renews ownership until ctx is done. On lost ownership (or repeated
// infrastructure errors) it cancels the callback context via the guard so the
// handler stops instead of acking a message it no longer owns.
func (c *Consumer) keeping(ctx context.Context, guard *ownerGuard, stream string, messageID string) {
	logger := c.logger.With(slog.String("stream", stream), slog.String("messageID", messageID))
	ticker := time.NewTicker(c.keepInterval)
	defer ticker.Stop()

	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		ok, err := c.keep(ctx, stream, messageID)
		if ok {
			failures = 0
			continue
		}
		if ctx.Err() != nil {
			// Renewal aborted because the message finished or the consumer is
			// closing; nothing to report.
			return
		}
		failures++
		logger.Warn("keep failed", slog.Int("failures", failures), slog.Any("err", err))
		if errors.Is(err, ErrOwnershipLost) || failures >= keepErrorRetries {
			guard.fail()
			return
		}
	}
}

// messageGroupHandler consumes one message in cluster mode.
func (c *Consumer) messageGroupHandler(parent context.Context, stream string, message redis.XMessage, callback mq.CallbackFunc) error {
	logger := c.logger.With(slog.String("stream", stream), slog.String("messageID", message.ID))

	if !c.beginHandler() {
		// Closing: leave the message pending for the next consumer.
		return nil
	}
	defer c.endHandler()

	var messageValues model.MessageValues
	if err := messageValues.ParseMap(message.Values); err != nil {
		// A malformed payload can never succeed; ack it so it does not poison
		// the pending list forever.
		logger.Error("parse message failed, acking", slog.Any("err", err))
		return c.ack(parent, stream, message.ID)
	}

	var labels []string
	if messageValues.Labels != "" {
		_ = json.Unmarshal([]byte(messageValues.Labels), &labels)
	}

	// The callback receives the consume deadline and is cancelled when the
	// consumer closes or when ownership is lost.
	base := parent
	if c.consumeTimeout > 0 {
		var timeoutCancel context.CancelFunc
		base, timeoutCancel = context.WithTimeout(parent, c.consumeTimeout)
		defer timeoutCancel()
	}
	ctx, cancel := context.WithCancel(base)
	defer cancel()

	guard := newOwnerGuard(cancel)

	c.keepers.Add(1)
	go func() {
		defer c.keepers.Done()
		c.keeping(ctx, guard, stream, message.ID)
	}()

	err := c.safeCallback(ctx, callback, &mq.MessageExt{
		Message: mq.Message{
			Topic:   messageValues.Topic,
			Labels:  labels,
			Content: []byte(messageValues.Content),
		},
		MsgId: message.ID,
	})

	if guard.Lost() {
		// Another consumer owns the message now; it must not be acked here.
		logger.Warn("callback interrupted by ownership loss, leaving message to new owner")
		return nil
	}

	if err != nil {
		if parent.Err() != nil {
			// Shutdown interrupted the callback: leave the message pending so
			// it is redelivered after idleTimeout.
			logger.Debug("callback interrupted by shutdown, leaving message pending")
			return nil
		}
		logger.Error("callback failed, message stays pending", slog.Any("err", err))
		return err
	}

	if ctx.Err() != nil && parent.Err() == nil {
		// The callback exceeded its consume deadline but reported success.
		// The work is durable, so acking is still correct.
		logger.Warn("callback finished after its deadline, acking anyway")
	}

	return c.ack(parent, stream, message.ID)
}

// ack acknowledges a message. It uses the subscription context (not the
// per-message deadline) and skips the call when the consumer is shutting down,
// so the message is redelivered instead of being acked by a dying process.
func (c *Consumer) ack(ctx context.Context, stream string, messageID string) error {
	if ctx.Err() != nil {
		return nil
	}
	if _, err := c.client.XAck(ctx, stream, c.group, messageID).Result(); err != nil {
		c.logger.Error("XACK failed",
			slog.String("stream", stream),
			slog.String("messageID", messageID),
			slog.Any("err", err),
		)
		return err
	}
	return nil
}

func (c *Consumer) messagesGroupHandler(ctx context.Context, stream string, messages []redis.XMessage, callback mq.CallbackFunc) error {
	for _, message := range messages {
		if ctx.Err() != nil || c.stopping() {
			return nil
		}
		if err := c.messageGroupHandler(ctx, stream, message, callback); err != nil {
			// 不尝试处理更多的消息
			return err
		}
	}
	return nil
}

func (c *Consumer) entriesGroupHandler(ctx context.Context, entries []redis.XStream, callback mq.CallbackFunc) error {
	for _, entry := range entries {
		if err := c.messagesGroupHandler(ctx, entry.Stream, entry.Messages, callback); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) streamGroupConsume(ctx context.Context, stream string, callback mq.CallbackFunc) error {
	logger := c.logger.With(slog.String("stream", stream))

	if c.stopping() || ctx.Err() != nil {
		return nil
	}

	entries, err := c.client.XReadGroup(ctx, &redis.XReadGroupArgs{
		Group:    c.group,
		Consumer: c.uniqueId,
		Streams:  []string{stream, ">"},
		Count:    1,
		Block:    c.blockTimeout, // 避免一直等待而无法处理其他队列
		NoAck:    false,
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			return nil
		}
		logger.Error("XREADGROUP failed", slog.Any("err", err))
		return err
	}
	return c.entriesGroupHandler(ctx, entries, callback)
}

// streamsGroupConsume consumes streams sequentially to avoid per-stream
// concurrency inside one subscription.
func (c *Consumer) streamsGroupConsume(ctx context.Context, streams []string, callback mq.CallbackFunc) error {
	for _, stream := range streams {
		if c.stopping() || ctx.Err() != nil {
			return nil
		}
		if err := c.streamGroupConsume(ctx, stream, callback); err != nil {
			return err
		}
	}
	return nil
}

// streamDeadLetterHandler transfers messages whose retry budget is exhausted to
// the DLQ stream.
func (c *Consumer) streamDeadLetterHandler(ctx context.Context, stream string) error {
	logger := c.logger.With(slog.String("stream", stream))

	pendings, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream:   stream,
		Group:    c.group,
		Idle:     c.idleTimeout,
		Start:    "-",
		End:      "+",
		Count:    10,
		Consumer: "",
	}).Result()
	if err != nil {
		logger.Error("XPENDING failed", slog.Any("err", err))
		return err
	}
	if len(pendings) <= 0 {
		return nil
	}

	dlIDs := make([]string, 0, len(pendings))
	attempts := make(map[string]int64, len(pendings))
	for _, pending := range pendings {
		// `pending.RetryCount` is actually the delivery count.
		if (pending.RetryCount - 1) < c.maxRetry {
			continue
		}
		dlIDs = append(dlIDs, pending.ID)
		attempts[pending.ID] = pending.RetryCount
	}
	if len(dlIDs) == 0 {
		return nil
	}
	logger.Debug("find dead letters", slog.Int("length", len(dlIDs)), slog.Any("ids", dlIDs))

	dlStream := c.stream2DeadLetterStream(stream)
	for _, id := range dlIDs {
		if ctx.Err() != nil {
			return nil
		}
		_, err := c.client.Eval(ctx, dlqTransferScript, []string{stream, dlStream},
			c.group, id, deadLetterReasonMaxRetry, fmt.Sprintf("%d", attempts[id]),
		).Result()
		if err != nil {
			logger.Error("DL transfer failed", slog.String("messageID", id), slog.Any("err", err))
			return err
		}
		logger.Debug("transferred dead letter", slog.String("messageID", id), slog.String("dlq", dlStream))
	}
	return nil
}

func (c *Consumer) streamsDeadLetterHandler(ctx context.Context, streams []string) error {
	for _, stream := range streams {
		if ctx.Err() != nil {
			return nil
		}
		if err := c.streamDeadLetterHandler(ctx, stream); err != nil {
			return err
		}
	}
	return nil
}

// streamRetry re-claims one idle pending message and redelivers it.
func (c *Consumer) streamRetry(ctx context.Context, stream string, callback mq.CallbackFunc) error {
	logger := c.logger.With(slog.String("stream", stream))

	pendings, err := c.client.XPendingExt(ctx, &redis.XPendingExtArgs{
		Stream:   stream,
		Group:    c.group,
		Idle:     c.idleTimeout,
		Start:    "-",
		End:      "+",
		Count:    1, // 每次只重试一条
		Consumer: "",
	}).Result()
	if err != nil {
		logger.Error("XPENDING failed", slog.Any("err", err))
		return err
	}
	if len(pendings) <= 0 {
		return nil
	}

	pending := pendings[0]
	if (pending.RetryCount - 1) >= c.maxRetry {
		// Retry budget exhausted; the DLQ handler owns this message.
		return nil
	}

	xmsgs, err := c.client.XClaim(ctx, &redis.XClaimArgs{
		Stream:   stream,
		Group:    c.group,
		Consumer: c.uniqueId,
		MinIdle:  c.idleTimeout,
		Messages: []string{pending.ID},
	}).Result()
	if err != nil {
		logger.Error("XCLAIM failed", slog.Any("err", err))
		return err
	}
	if len(xmsgs) == 0 {
		// Someone else claimed it first.
		return nil
	}
	return c.messagesGroupHandler(ctx, stream, xmsgs, callback)
}

func (c *Consumer) streamsRetry(ctx context.Context, streams []string, callback mq.CallbackFunc) error {
	for _, stream := range streams {
		if ctx.Err() != nil || c.stopping() {
			return nil
		}
		if err := c.streamRetry(ctx, stream, callback); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) clusterConsume(ctx context.Context, streams []string, callback mq.CallbackFunc) error {
	// 如果有能力消费，优先尝试处理常规队列
	if err := c.streamsGroupConsume(ctx, streams, callback); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		c.logger.Error("consume failed", slog.Any("err", err))
		return err
	}

	// 处理死信
	if err := c.streamsDeadLetterHandler(ctx, streams); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		// 处理死信失败可能是 Redis 连接问题，需要等待一会再重试
		return err
	}

	// Retry 要放在最后，避免遇到死信时反复失败
	if err := c.streamsRetry(ctx, streams, callback); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	return nil
}
