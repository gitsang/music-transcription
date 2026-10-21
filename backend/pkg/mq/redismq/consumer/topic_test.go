package consumer

import (
	"testing"
)

func TestTopicStreamTransform(t *testing.T) {
	namespaces := []string{"ns1", "ns2"}
	c := NewConsumer(
		WithNamespaces(namespaces),
	)

	if got, want := c.topic2Stream("topic1"), "ns1:ns2:Q:topic1"; got != want {
		t.Fatalf("topic2Stream() = %q, want %q", got, want)
	}
	if got, want := c.stream2Topic("ns1:ns2:Q:topic1"), "topic1"; got != want {
		t.Fatalf("stream2Topic() = %q, want %q", got, want)
	}
	if got, want := c.topic2DeadLetterStream("topic1"), "ns1:ns2:DLQ:topic1"; got != want {
		t.Fatalf("topic2DeadLetterStream() = %q, want %q", got, want)
	}
	if got, want := c.stream2DeadLetterStream("ns1:ns2:Q:topic1"), "ns1:ns2:DLQ:topic1"; got != want {
		t.Fatalf("stream2DeadLetterStream() = %q, want %q", got, want)
	}
}

func TestTopic2StreamKeepsLabelsAndGlobs(t *testing.T) {
	c := NewConsumer(WithNamespaces([]string{"music-transcription"}))
	if got, want := c.topic2Stream("transcription.jobs:YQ=="), "music-transcription:Q:transcription.jobs:YQ=="; got != want {
		t.Fatalf("topic2Stream() = %q, want %q", got, want)
	}
	if got, want := c.topic2Stream("transcription.*"), "music-transcription:Q:transcription.*"; got != want {
		t.Fatalf("topic2Stream() = %q, want %q", got, want)
	}
}

func TestHasGlob(t *testing.T) {
	cases := map[string]bool{
		"transcription.jobs":      false,
		"transcription.jobs:YQ==": false,
		"transcription.*":         true,
		"transcription.??bs":      true,
		"transcription.[ab]":      true,
		`transcription.\*`:        true,
	}
	for topic, want := range cases {
		if got := hasGlob(topic); got != want {
			t.Errorf("hasGlob(%q) = %v, want %v", topic, got, want)
		}
	}
}

func TestSplitTopics(t *testing.T) {
	got := splitTopics(" a , b ,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitTopics() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitTopics() = %v, want %v", got, want)
		}
	}
}

func TestBackoffCaps(t *testing.T) {
	var b backoff
	first := b.next()
	if first != minBackoff {
		t.Fatalf("first backoff = %v, want %v", first, minBackoff)
	}
	for i := 0; i < 20; i++ {
		b.next()
	}
	if b.current != maxBackoff {
		t.Fatalf("backoff must cap at %v, got %v", maxBackoff, b.current)
	}
	b.reset()
	if b.next() != minBackoff {
		t.Fatal("reset must restore the minimum backoff")
	}
}

func TestConsumeFromWhereString(t *testing.T) {
	c := NewConsumer()
	if c.mode != ConsumeModeCluster {
		t.Fatalf("default mode = %q, want cluster", c.mode)
	}
	if c.consumerOptions.consumeTimeout <= 0 {
		t.Fatal("default consume timeout must be positive")
	}
	if c.closeTimeout <= 0 {
		t.Fatal("default close timeout must be positive")
	}
}

func TestConsumerGroupStartOffsetIsZero(t *testing.T) {
	// Documented contract: a new group replays already queued messages.
	if groupStartOffset != "0" {
		t.Fatalf("groupStartOffset = %q, want \"0\"", groupStartOffset)
	}
}
