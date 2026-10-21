package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	"github.com/gitsang/music-transcription/backend/pkg/mq/redismq/model"
	"github.com/go-redis/redis/v8"
)

// findLatestID returns the last entry id of a stream, or "" when empty.
func (c *Consumer) findLatestID(ctx context.Context, stream string) (string, error) {
	entries, err := c.client.XRevRangeN(ctx, stream, "+", "-", 1).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return "", nil
		}
		c.logger.Error("XREVRANGE failed", slog.String("stream", stream), slog.Any("err", err))
		return "", err
	}
	if len(entries) == 0 {
		return "", nil
	}
	return entries[0].ID, nil
}

// loadOffset returns the read offset for a broadcast stream.
//
// Definition of the start position (explicit, upstream was implicit):
//   - CONSUME_FROM_FIRST_OFFSET starts at "0-0", replaying the whole stream;
//   - CONSUME_FROM_LAST_OFFSET starts at the current last entry id, so only
//     messages published after the subscription starts are seen.
//
// The offset is then advanced in memory by every consumed message; broadcast
// mode has no consumer group and therefore no server-side cursor.
func (c *Consumer) loadOffset(ctx context.Context, stream string) (string, error) {
	if offsetAny, ok := c.offsets.Load(stream); ok {
		if offset, ok := offsetAny.(string); ok {
			return offset, nil
		}
	}

	var offset string
	switch c.consumeFromWhere {
	case mq.ConsumeFromFirstOffset:
		offset = "0-0"
	case mq.ConsumeFromLastOffset:
		lastID, err := c.findLatestID(ctx, stream)
		if err != nil {
			return "", err
		}
		if lastID == "" {
			offset = "0-0"
		} else {
			offset = lastID
		}
	default:
		return "", errors.New("mq: unsupported consume from where")
	}

	c.offsets.Store(stream, offset)
	return offset, nil
}

func (c *Consumer) messageHandler(ctx context.Context, stream string, message redis.XMessage, callback mq.CallbackFunc) error {
	if !c.beginHandler() {
		return nil
	}
	defer c.endHandler()

	var messageValues model.MessageValues
	if err := messageValues.ParseMap(message.Values); err != nil {
		c.logger.Error("parse message failed", slog.Any("err", err))
		// Advance the offset so a malformed entry cannot block the stream.
		c.offsets.Store(stream, message.ID)
		return nil
	}

	var labels []string
	if messageValues.Labels != "" {
		_ = json.Unmarshal([]byte(messageValues.Labels), &labels)
	}

	handlerCtx := ctx
	if c.consumeTimeout > 0 {
		var cancel context.CancelFunc
		handlerCtx, cancel = context.WithTimeout(ctx, c.consumeTimeout)
		defer cancel()
	}

	err := c.safeCallback(handlerCtx, callback, &mq.MessageExt{
		Message: mq.Message{
			Topic:   messageValues.Topic,
			Labels:  labels,
			Content: []byte(messageValues.Content),
		},
		MsgId: message.ID,
	})
	if err != nil {
		c.logger.Error("callback failed", slog.String("stream", stream), slog.Any("err", err))
		return err
	}

	c.offsets.Store(stream, message.ID)
	return nil
}

func (c *Consumer) messagesHandler(ctx context.Context, stream string, messages []redis.XMessage, callback mq.CallbackFunc) error {
	for _, message := range messages {
		if ctx.Err() != nil || c.stopping() {
			return nil
		}
		if err := c.messageHandler(ctx, stream, message, callback); err != nil {
			// 不尝试处理更多的消息
			return err
		}
	}
	return nil
}

func (c *Consumer) entriesHandler(ctx context.Context, entries []redis.XStream, callback mq.CallbackFunc) error {
	for _, entry := range entries {
		if err := c.messagesHandler(ctx, entry.Stream, entry.Messages, callback); err != nil {
			return err
		}
	}
	return nil
}

func (c *Consumer) streamConsume(ctx context.Context, stream string, callback mq.CallbackFunc) error {
	if c.stopping() || ctx.Err() != nil {
		return nil
	}

	offset, err := c.loadOffset(ctx, stream)
	if err != nil {
		return err
	}

	entries, err := c.client.XRead(ctx, &redis.XReadArgs{
		Streams: []string{stream, offset},
		Count:   1,
		Block:   c.blockTimeout, // 避免一直等待而无法处理其他队列
	}).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, context.Canceled) {
			return nil
		}
		c.logger.Error("XREAD failed", slog.String("stream", stream), slog.Any("err", err))
		return err
	}

	// Broadcast mode re-reads from the stored id; advance past it so the same
	// entry is not returned twice (XREAD is inclusive).
	for i := range entries {
		entry := &entries[i]
		skip := 0
		for skip < len(entry.Messages) && entry.Messages[skip].ID <= offset {
			skip++
		}
		entry.Messages = entry.Messages[skip:]
	}

	return c.entriesHandler(ctx, entries, callback)
}

func (c *Consumer) streamsConsume(ctx context.Context, streams []string, callback mq.CallbackFunc) error {
	for _, stream := range streams {
		if c.stopping() || ctx.Err() != nil {
			return nil
		}
		if err := c.streamConsume(ctx, stream, callback); err != nil {
			return err
		}
	}
	return nil
}

// broadcastConsume reads every stream without a consumer group: each subscriber
// sees every message. There is no retry or dead letter handling in this mode
// (an entry cannot be re-delivered to only one subscriber).
func (c *Consumer) broadcastConsume(ctx context.Context, streams []string, callback mq.CallbackFunc) error {
	if err := c.streamsConsume(ctx, streams, callback); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		c.logger.Error("broadcast consume failed", slog.Any("err", err))
		return err
	}
	return nil
}
