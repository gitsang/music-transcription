package consumer

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq/redismq/util"
)

// groupStartOffset is the stream id used when a consumer group is created.
//
// Explicit definition (upstream always used "0" implicitly):
//   - cluster mode: a brand new group always starts at "0", i.e. it replays
//     messages that were published before the group existed. This is the
//     upstream behaviour and it is kept on purpose because dropping already
//     queued work on a first deployment would lose jobs. ConsumeFromWhere
//     therefore only affects broadcast mode.
//   - broadcast mode: the read offset honours ConsumeFromWhere (see
//     loadOffset), a group is never created.
const groupStartOffset = "0"

// hasGlob reports whether topic contains Redis glob metacharacters.
func hasGlob(topic string) bool {
	return strings.ContainsAny(topic, "*?[]\\")
}

func splitTopics(topicPattern string) []string {
	raw := strings.Split(topicPattern, ",")
	topics := make([]string, 0, len(raw))
	for _, topic := range raw {
		topic = strings.TrimSpace(topic)
		if topic == "" {
			continue
		}
		topics = append(topics, topic)
	}
	return topics
}

// utilGenTopicCombineWithLabels expands the label power set. Kept as a small
// wrapper so the subscribe path and tests share one implementation.
func utilGenTopicCombineWithLabels(topics []string, labels []string) []string {
	return util.GenTopicCombineWithLabels(topics, labels)
}

// discoveryCache memoises glob resolution so the consume loop does not SCAN
// on every iteration.
type discoveryCache struct {
	mu      sync.Mutex
	entries map[string]discoveryEntry
}

type discoveryEntry struct {
	streams []string
	at      time.Time
}

func (d *discoveryCache) get(key string, ttl time.Duration) ([]string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		return nil, false
	}
	entry, ok := d.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.at) > ttl {
		return nil, false
	}
	return entry.streams, true
}

func (d *discoveryCache) put(key string, streams []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.entries == nil {
		d.entries = make(map[string]discoveryEntry)
	}
	d.entries[key] = discoveryEntry{streams: streams, at: time.Now()}
}

func (c *Consumer) topic2Stream(topic string) string {
	parts := make([]string, 0, len(c.namespaces)+2)
	parts = append(parts, c.namespaces...)
	parts = append(parts, "Q", topic)
	return strings.Join(parts, ":")
}

func (c *Consumer) topics2Streams(topics []string) []string {
	streams := make([]string, 0, len(topics))
	seen := make(map[string]struct{}, len(topics))
	for _, topic := range topics {
		stream := c.topic2Stream(topic)
		if _, ok := seen[stream]; ok {
			continue
		}
		seen[stream] = struct{}{}
		streams = append(streams, stream)
	}
	return streams
}

func (c *Consumer) topic2DeadLetterStream(topic string) string {
	parts := make([]string, 0, len(c.namespaces)+2)
	parts = append(parts, c.namespaces...)
	parts = append(parts, "DLQ", topic)
	return strings.Join(parts, ":")
}

func (c *Consumer) streamPrefix() string {
	parts := make([]string, 0, len(c.namespaces)+1)
	parts = append(parts, c.namespaces...)
	parts = append(parts, "Q")
	return strings.Join(parts, ":") + ":"
}

func (c *Consumer) stream2Topic(stream string) string {
	return strings.TrimPrefix(stream, c.streamPrefix())
}

func (c *Consumer) stream2DeadLetterStream(stream string) string {
	return c.topic2DeadLetterStream(c.stream2Topic(stream))
}

// resolveStreams maps one topic selector (exact topic or glob) to stream names.
//
// Exact topics are derived directly from the namespace, never via Redis
// lookups, so a subscription works before anyone publishes. Glob patterns are
// resolved with SCAN (never KEYS) and cached for topicRefreshInterval.
func (c *Consumer) resolveStreams(ctx context.Context, topic string) ([]string, error) {
	if !hasGlob(topic) {
		return []string{c.topic2Stream(topic)}, nil
	}

	pattern := c.topic2Stream(topic)
	if cached, ok := c.discovery.get(pattern, c.topicRefreshInterval); ok {
		return cached, nil
	}

	streams := make([]string, 0, 8)
	seen := make(map[string]struct{})
	var cursor uint64
	for {
		keys, next, err := c.client.Scan(ctx, cursor, pattern, 512).Result()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return nil, err
			}
			c.logger.Error("SCAN failed", slog.String("pattern", pattern), slog.Any("err", err))
			return nil, err
		}
		for _, key := range keys {
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			streams = append(streams, key)
		}
		cursor = next
		if cursor == 0 {
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}

	c.discovery.put(pattern, streams)
	return streams, nil
}

// matchTopics resolves a list of topic selectors into streams.
func (c *Consumer) matchTopics(ctx context.Context, topics []string) ([]string, error) {
	streams := make([]string, 0, len(topics))
	for _, topic := range topics {
		resolved, err := c.resolveStreams(ctx, topic)
		if err != nil {
			return nil, err
		}
		streams = append(streams, resolved...)
	}
	return dedupeStrings(streams), nil
}

func dedupeStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// makeStreams creates the consumer group for every stream (cluster mode).
func (c *Consumer) makeStreams(ctx context.Context, streams []string) error {
	logger := c.logger.With(slog.Any("streams", streams), slog.Any("group", c.group))
	for _, stream := range streams {
		if _, ok := c.ensured.Load(stream); ok {
			continue
		}
		err := c.client.XGroupCreateMkStream(ctx, stream, c.group, groupStartOffset).Err()
		if err != nil {
			if isBusyGroup(err) {
				c.ensured.Store(stream, struct{}{})
				continue
			}
			logger.Error("XGROUP CREATE MKSTREAM failed", slog.Any("err", err))
			return err
		}
		c.ensured.Store(stream, struct{}{})
		logger.Info("create stream group success")
	}
	return nil
}

func isBusyGroup(err error) bool {
	return strings.Contains(err.Error(), "BUSYGROUP") ||
		strings.Contains(err.Error(), "already exists")
}
