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
//     enforces the configured close timeout with a real cancellation;
//   - the dead letter transfer is a single Lua script (XADD + XACK), so a
//     crash cannot duplicate the DLQ entry or leave the message pending.
package consumer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
	"github.com/gitsang/music-transcription/backend/pkg/mq/log/nop"
	"github.com/go-redis/redis/v8"
)

var (
	// ErrClosed is returned when the consumer is used after Close.
	ErrClosed = errors.New("mq: consumer is closed")
	// ErrNotConnected is returned when Subscribe is called before Connect.
	ErrNotConnected = errors.New("mq: consumer is not connected")
	// ErrOwnershipLost means the pending message was claimed by another
	// consumer (or removed). The callback must not ack it.
	ErrOwnershipLost = errors.New("mq: message ownership lost")
)

type Consumer struct {
	*consumerOptions
	logger *slog.Logger
	client redis.UniversalClient

	globalCtx context.Context

	mu        sync.Mutex
	runCtx    context.Context
	runCancel context.CancelFunc
	stopCh    chan struct{}
	stopOnce  sync.Once
	connected bool
	closed    bool

	// gate serialises handler admission with Close so no callback can start
	// after Close began waiting for in-flight work.
	gate          sync.Mutex
	handlerClosed bool
	// inflight tracks message handlers currently running.
	inflight sync.WaitGroup
	// inflightCount mirrors inflight for cheap observation.
	inflightCount atomic.Int64
	// keepers tracks keep-alive goroutines. Adds only happen from admitted
	// handlers, which Close waits for before waiting on keepers.
	keepers sync.WaitGroup
	// loops tracks every Subscribe goroutine.
	loops sync.WaitGroup

	// offsets stores broadcast read offsets per stream.
	offsets sync.Map
	// ensured records streams whose consumer group already exists.
	ensured sync.Map

	discovery discoveryCache
}

var _ mq.Consumer = &Consumer{}

func defaultConsumer() *Consumer {
	return &Consumer{
		consumerOptions: defaultConsumerOptions(),
		logger:          slog.New(nop.New()),
		client:          nil,
		globalCtx:       context.Background(),
		runCtx:          nil,
		runCancel:       nil,
		stopCh:          make(chan struct{}),
	}
}

func WithContext(ctx context.Context) Option {
	return func(c *Consumer) {
		if ctx != nil {
			c.globalCtx = ctx
		}
	}
}

func WithLogHandler(h slog.Handler) Option {
	return func(c *Consumer) {
		if h != nil {
			c.logger = slog.New(h)
		}
	}
}

func WithClient(client redis.UniversalClient) Option {
	return func(c *Consumer) {
		c.client = client
	}
}

func NewConsumer(opts ...Option) *Consumer {
	c := defaultConsumer()
	for _, apply := range opts {
		apply(c)
	}
	c.logger = c.logger.With(slog.String("uniqueId", c.uniqueId))
	return c
}

// Connect checks the Redis connection and prepares the running context.
func (c *Consumer) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return ErrClosed
	}
	if c.connected {
		return nil
	}
	if c.client == nil {
		return errors.New("mq: redis client is nil")
	}

	runCtx, runCancel := context.WithCancel(c.globalCtx)
	if err := c.client.Ping(runCtx).Err(); err != nil {
		runCancel()
		return fmt.Errorf("mq: ping redis failed: %w", err)
	}
	c.runCtx, c.runCancel = runCtx, runCancel
	c.connected = true
	return nil
}

// Close stops pulling new messages, waits for in-flight callbacks (bounded by
// closeTimeout) and then cancels them for real.
//
// closeTimeout semantics (unchanged from upstream):
//   - 0: wait forever for in-flight callbacks;
//   - <0: cancel in-flight callbacks immediately;
//   - >0: wait up to that duration, then cancel.
func (c *Consumer) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	connected := c.connected
	runCancel := c.runCancel
	c.mu.Unlock()

	if !connected {
		return nil
	}

	// Phase 0: forbid new callbacks from starting.
	c.gate.Lock()
	c.handlerClosed = true
	c.gate.Unlock()

	// Phase 1: stop accepting / pulling new work. In-flight callbacks keep
	// running on purpose so they can ack their durable outcome.
	c.stopOnce.Do(func() { close(c.stopCh) })

	// Phase 2: wait for in-flight callbacks within the close timeout.
	drained := c.waitInflight(c.closeTimeout)
	if drained {
		c.logger.Debug("consumer drained", slog.Int64("inflight", c.inflightCount.Load()))
	} else {
		// Pending messages stay in the PEL and will be retried by another
		// consumer after idleTimeout.
		c.logger.Warn("consumer close timeout exceeded, cancelling in-flight callbacks",
			slog.Duration("closeTimeout", c.closeTimeout),
			slog.Int64("inflight", c.inflightCount.Load()),
		)
	}

	// Phase 3: cancel the run context (callbacks, keepers and read loops).
	if runCancel != nil {
		runCancel()
	}
	if !c.waitLoops(c.loopWaitTimeout()) {
		c.logger.Warn("consumer subscribe loops did not exit")
	}
	c.waitKeepers()
	return nil
}

// loopWaitTimeout bounds the wait for subscribe goroutines after cancellation.
func (c *Consumer) loopWaitTimeout() time.Duration {
	if c.closeTimeout > 0 {
		return c.closeTimeout
	}
	return 5 * time.Second
}

func (c *Consumer) waitInflight(timeout time.Duration) bool {
	if timeout < 0 {
		return false
	}
	done := make(chan struct{})
	go func() {
		c.inflight.Wait()
		close(done)
	}()
	if timeout == 0 {
		<-done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (c *Consumer) waitLoops(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		c.loops.Wait()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (c *Consumer) waitKeepers() {
	done := make(chan struct{})
	go func() {
		c.keepers.Wait()
		close(done)
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		c.logger.Warn("keep-alive goroutines did not exit")
	}
}

// beginHandler admits one message handler. It returns false when the consumer
// is closing, in which case the message must be left unacked.
func (c *Consumer) beginHandler() bool {
	c.gate.Lock()
	defer c.gate.Unlock()
	if c.handlerClosed {
		return false
	}
	c.inflight.Add(1)
	c.inflightCount.Add(1)
	return true
}

func (c *Consumer) endHandler() {
	c.inflightCount.Add(-1)
	c.inflight.Done()
}

// stopping reports whether new messages may still be picked up.
func (c *Consumer) stopping() bool {
	select {
	case <-c.stopCh:
		return true
	default:
		return false
	}
}

// Inflight returns how many callbacks are currently running.
func (c *Consumer) Inflight() int64 {
	return c.inflightCount.Load()
}

// runningContext returns the subscription context.
func (c *Consumer) runningContext() (context.Context, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected {
		return nil, ErrNotConnected
	}
	return c.runCtx, nil
}

// safeCallback runs the user callback with panic protection. A panic is
// converted into an error, which means the message is NOT acked and follows
// the normal retry / dead letter path.
func (c *Consumer) safeCallback(ctx context.Context, callback mq.CallbackFunc, msgs ...*mq.MessageExt) (err error) {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("consumer callback panicked",
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
			err = fmt.Errorf("mq: callback panic: %v", r)
		}
	}()
	return callback(ctx, msgs...)
}
