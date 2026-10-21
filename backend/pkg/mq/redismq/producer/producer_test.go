package producer

import (
	"errors"
	"testing"

	"github.com/gitsang/music-transcription/backend/pkg/mq"
)

func TestDefaultProducerDoesNotTrim(t *testing.T) {
	p := NewProducer()
	if p.streamMaxLen != 0 {
		t.Fatalf("default streamMaxLen = %d, want 0 (no trimming)", p.streamMaxLen)
	}
	_, args, err := p.xaddArgs(&mq.Message{Topic: "t", Content: []byte("x")})
	if err != nil {
		t.Fatalf("xaddArgs: %v", err)
	}
	if args.MaxLen != 0 || args.MaxLenApprox != 0 || args.MinID != "" {
		t.Fatalf("default XADD must not trim, got MaxLen=%d MaxLenApprox=%d MinID=%q",
			args.MaxLen, args.MaxLenApprox, args.MinID)
	}
	if args.Approx {
		t.Fatal("default XADD must not use approximate trimming")
	}
}

func TestExplicitMaxLen(t *testing.T) {
	p := NewProducer(WithStreamMaxLen(500), WithApproximateTrim(true))
	_, args, err := p.xaddArgs(&mq.Message{Topic: "t"})
	if err != nil {
		t.Fatalf("xaddArgs: %v", err)
	}
	if args.MaxLen != 500 || !args.Approx {
		t.Fatalf("MaxLen=%d Approx=%v, want 500/true", args.MaxLen, args.Approx)
	}
}

func TestZeroMaxLenDisablesTrim(t *testing.T) {
	p := NewProducer(WithStreamMaxLen(1000), WithStreamMaxLen(0))
	if p.streamMaxLen != 0 {
		t.Fatalf("streamMaxLen = %d, want 0", p.streamMaxLen)
	}
	_, args, _ := p.xaddArgs(&mq.Message{Topic: "t"})
	if args.MaxLen != 0 {
		t.Fatalf("MaxLen = %d, want 0", args.MaxLen)
	}
}

func TestMinIDTrimming(t *testing.T) {
	p := NewProducer(WithStreamMaxLen(100), WithStreamMinID("1700000000000-0"))
	_, args, _ := p.xaddArgs(&mq.Message{Topic: "t"})
	if args.MinID != "1700000000000-0" {
		t.Fatalf("MinID = %q", args.MinID)
	}
	if args.MaxLen != 0 {
		t.Fatalf("MaxLen must be cleared when MinID is set, got %d", args.MaxLen)
	}
}

func TestTopicStream(t *testing.T) {
	p := NewProducer(WithNamespaces([]string{"music-transcription"}))
	stream, _, err := p.xaddArgs(&mq.Message{Topic: "transcription.jobs", Labels: []string{"b", "a"}})
	if err != nil {
		t.Fatalf("xaddArgs: %v", err)
	}
	if want := "music-transcription:Q:transcription.jobs:YSxi"; stream != want {
		t.Fatalf("stream = %q, want %q", stream, want)
	}
	if got := p.stream2Topic(stream); got != "transcription.jobs:YSxi" {
		t.Fatalf("stream2Topic() = %q", got)
	}
}

func TestPublishBeforeConnect(t *testing.T) {
	p := NewProducer()
	err := p.Publish(t.Context(), &mq.Message{Topic: "t"})
	if !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Publish before Connect = %v, want ErrNotConnected", err)
	}
}

func TestPublishAfterClose(t *testing.T) {
	p := NewProducer()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := p.Publish(t.Context(), &mq.Message{Topic: "t"})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
}

func TestNilMessageIsRejected(t *testing.T) {
	p := NewProducer()
	// Simulate a connected producer without Redis.
	p.connected = true
	p.client = nil
	// connected without client still reports not connected (guard).
	if err := p.Publish(t.Context(), nil); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Publish with nil client = %v, want ErrNotConnected", err)
	}
}

func TestPublishErrorWrapping(t *testing.T) {
	inner := errors.New("boom")
	err := error(&publishError{index: 1, total: 3, err: inner})
	if !errors.Is(err, ErrBulkSendFailed) {
		t.Fatal("publishError must match ErrBulkSendFailed")
	}
	if !errors.Is(err, inner) {
		t.Fatal("publishError must unwrap the cause")
	}
}
