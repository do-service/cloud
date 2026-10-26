package api

import (
	"context"
	"io"

	"golang.org/x/time/rate"
)

// NewLimiter is a bandwidth limit of bytesPerSec for Options.UploadLimit
// or DownloadLimit; 0 or less is no limit (nil). Its burst is a quarter
// second's worth (at least 32 KiB): small enough that the rate looks
// even, big enough that a fast link isn't throttled by the wait calls.
func NewLimiter(bytesPerSec int) *rate.Limiter {
	if bytesPerSec <= 0 {
		return nil
	}
	return rate.NewLimiter(rate.Limit(bytesPerSec), max(bytesPerSec/4, 32<<10))
}

// limitReader reads from r no faster than lim allows.
func limitReader(ctx context.Context, r io.Reader, lim *rate.Limiter) io.Reader {
	return &limitedReader{ctx: ctx, r: r, lim: lim}
}

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	lim *rate.Limiter
}

func (l *limitedReader) Read(p []byte) (int, error) {
	// Never more than one burst at a time: WaitN refuses bigger asks.
	if b := l.lim.Burst(); len(p) > b {
		p = p[:b]
	}
	n, err := l.r.Read(p)
	if n > 0 {
		if werr := l.lim.WaitN(l.ctx, n); werr != nil && err == nil {
			err = werr
		}
	}
	return n, err
}
