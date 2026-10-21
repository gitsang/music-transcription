// Package nop provides a silent slog.Handler used as the default logger of
// the mq packages.
//
// It is defined in this repository for local use.
package nop

import (
	"context"
	"log/slog"
)

type NopHandler struct {
}

var _ slog.Handler = new(NopHandler)

func New() *NopHandler {
	return &NopHandler{}
}

func (h *NopHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *NopHandler) Handle(context.Context, slog.Record) error {
	return nil
}

func (h *NopHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return h
}

func (h *NopHandler) WithGroup(name string) slog.Handler {
	return h
}
