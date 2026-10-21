package producer

import "time"

type producerOptions struct {
	// streamMaxLen is the MAXLEN (or MINID) trim target.
	//
	// 0 (the default) means "no trimming": the stream keeps every entry.
	// Upstream defaulted to 10000 with approximate trimming, which silently
	// dropped pending jobs from a work queue. Trimming is opt-in.
	streamMaxLen int64

	// streamMinID trims by minimum id instead of length when set.
	streamMinID string

	// approximate uses "~" instead of "=" for MAXLEN/MINID.
	approximate bool

	// limit is the MAXLEN/MINID LIMIT argument (0 = server default).
	limit int64

	namespaces []string

	// operationTimeout bounds a single publish when the caller context has no
	// deadline of its own.
	operationTimeout time.Duration
}

func defaultProducerOptions() *producerOptions {
	return &producerOptions{
		streamMaxLen:     0, // no trimming by default
		streamMinID:      "",
		approximate:      false,
		limit:            0,
		namespaces:       nil,
		operationTimeout: 5 * time.Second,
	}
}

type Option func(*Producer)

// WithStreamMaxLen enables length based trimming. Values <= 0 disable
// trimming (the default), so a setter of 0 is a no-op instead of a 10000
// default.
func WithStreamMaxLen(length int64) Option {
	return func(p *Producer) {
		if length < 0 {
			return
		}
		p.streamMaxLen = length
		if length > 0 {
			p.streamMinID = ""
		}
	}
}

// WithStreamMinID enables id based trimming (MINID), which is the safe way to
// bound a work queue: it never drops entries that were not yet processed.
func WithStreamMinID(minID string) Option {
	return func(p *Producer) {
		p.streamMinID = minID
		if minID != "" {
			p.streamMaxLen = 0
		}
	}
}

// WithApproximateTrim selects "~" trimming (cheaper, less exact).
func WithApproximateTrim(approximate bool) Option {
	return func(p *Producer) {
		p.approximate = approximate
	}
}

// WithTrimLimit sets the LIMIT argument used with approximate trimming.
func WithTrimLimit(limit int64) Option {
	return func(p *Producer) {
		if limit >= 0 {
			p.limit = limit
		}
	}
}

func WithNamespaces(namespaces []string) Option {
	return func(p *Producer) {
		p.namespaces = namespaces
	}
}

// WithOperationTimeout bounds each publish when ctx has no deadline.
func WithOperationTimeout(timeout time.Duration) Option {
	return func(p *Producer) {
		if timeout > 0 {
			p.operationTimeout = timeout
		}
	}
}
