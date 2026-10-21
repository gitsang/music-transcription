// Package producer implements a Redis Streams producer with topic-to-stream
// naming, labels and batch publish.
//
// A work queue must not silently drop entries, so no maximum length is applied
// by default. Trimming is opt-in via WithStreamMaxLen / WithStreamMinID.
package producer

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	"github.com/gitsang/music-transcription/backend/pkg/mq/log/nop"
	"github.com/gitsang/music-transcription/backend/pkg/mq/redismq/model"
	"github.com/gitsang/music-transcription/backend/pkg/mq/redismq/util"
	"github.com/go-redis/redis/v8"
)

type Producer struct {
	*producerOptions
	logger *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	client redis.UniversalClient

	mu        sync.Mutex
	connected bool
	closed    bool
}

var _ mq.Producer = &Producer{}

func WithContext(ctx context.Context) Option {
	return func(p *Producer) {
		if ctx != nil {
			p.ctx = ctx
		}
	}
}

func WithLogHandler(h slog.Handler) Option {
	return func(p *Producer) {
		if h != nil {
			p.logger = slog.New(h)
		}
	}
}

func WithClient(client redis.UniversalClient) Option {
	return func(p *Producer) {
		p.client = client
	}
}

func defaultProducer() *Producer {
	return &Producer{
		producerOptions: defaultProducerOptions(),
		logger:          slog.New(nop.New()),
		ctx:             context.Background(),
		cancel:          nil,
		client:          nil,
	}
}

func NewProducer(opts ...Option) *Producer {
	p := defaultProducer()
	for _, apply := range opts {
		apply(p)
	}
	return p
}

func (p *Producer) Connect() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return ErrClosed
	}
	if p.connected {
		return nil
	}
	if p.client == nil {
		return errors.New("mq: redis client is nil")
	}

	ctx, cancel := context.WithCancel(p.ctx)
	if err := p.client.Ping(ctx).Err(); err != nil {
		cancel()
		return err
	}
	p.ctx, p.cancel = ctx, cancel
	p.connected = true
	return nil
}

func (p *Producer) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	return nil
}

// xaddArgs builds the XADD arguments for one message. Trimming arguments are
// only added when explicitly configured, so streamMaxLen == 0 and
// streamMinID == "" produce an unbounded XADD.
func (p *Producer) xaddArgs(msg *mq.Message) (string, *redis.XAddArgs, error) {
	labels, err := json.Marshal(msg.Labels)
	if err != nil {
		return "", nil, err
	}
	stream := p.topic2Stream(util.GenRealTopicWithLabels(msg.Topic, msg.Labels))
	args := &redis.XAddArgs{
		Stream:     stream,
		NoMkStream: false,
		Approx:     p.approximate,
		Limit:      p.limit,
		ID:         "",
		Values: model.MessageValues{
			Topic:   msg.Topic,
			Labels:  string(labels),
			Content: string(msg.Content),
		}.ToMap(),
	}
	if p.streamMinID != "" {
		args.MinID = p.streamMinID
	} else if p.streamMaxLen > 0 {
		args.MaxLen = p.streamMaxLen
	}
	return stream, args, nil
}

// Publish writes every message to its stream in order, stopping at the first
// failure. Messages already written stay written (no batch rollback, matching
// upstream semantics).
func (p *Producer) Publish(ctx context.Context, msgs ...*mq.Message) error {
	p.mu.Lock()
	client := p.client
	connected := p.connected
	closed := p.closed
	baseCtx := p.ctx
	p.mu.Unlock()

	if closed {
		return ErrClosed
	}
	if !connected || client == nil {
		return ErrNotConnected
	}
	if ctx == nil {
		ctx = baseCtx
	}
	// Bind the caller context to the producer lifetime so Close aborts
	// in-flight publishes.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(baseCtx, cancel)
	defer stop()
	if p.operationTimeout > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, p.operationTimeout)
		defer timeoutCancel()
	}

	for idx, msg := range msgs {
		if msg == nil {
			return &publishError{index: idx, total: len(msgs), err: errors.New("nil message")}
		}
		if err := ctx.Err(); err != nil {
			return &publishError{index: idx, total: len(msgs), err: err}
		}
		stream, args, err := p.xaddArgs(msg)
		if err != nil {
			return &publishError{index: idx, total: len(msgs), err: err}
		}
		if err := client.XAdd(ctx, args).Err(); err != nil {
			p.logger.Error("publish failed",
				slog.Int("idx", idx),
				slog.String("stream", stream),
				slog.Any("err", err),
			)
			return &publishError{index: idx, total: len(msgs), err: err}
		}
	}
	return nil
}
