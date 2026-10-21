// Package mq defines the message queue front-end contract.
//
// It is defined in this repository so callers depend only on a stable local
// interface.
package mq

import (
	"context"
)

type Message struct {
	Topic   string
	Labels  []string
	Content []byte
}

type MessageExt struct {
	Message
	MsgId string
}

type ConsumeFromWhere int

const (
	ConsumeFromLastOffset ConsumeFromWhere = iota
	ConsumeFromFirstOffset
)

func (cfw ConsumeFromWhere) String() string {
	switch cfw {
	case ConsumeFromLastOffset:
		return "CONSUME_FROM_LAST_OFFSET"
	case ConsumeFromFirstOffset:
		return "CONSUME_FROM_FIRST_OFFSET"
	default:
		return ""
	}
}

type Producer interface {
	Connect() error
	Publish(ctx context.Context, msgs ...*Message) error
	Close() error
}

type CallbackFunc func(context.Context, ...*MessageExt) error

type Consumer interface {
	Connect() error
	Subscribe(topicPattern string, labels []string, callback CallbackFunc) error
	Close() error
}

type MessageQueue interface {
	Consumer(uniqueId string) Consumer
	Producer(uniqueId string) Producer
}
