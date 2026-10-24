package main

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
)

// countWriter counts bytes that were successfully written.
type countWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n.Add(int64(n))
	return n, err
}

// ctxReader makes a long read stop promptly on Ctrl-C.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// asyncWriter hands writes to a background goroutine through a bounded queue.
// While one chunk is travelling over the network, the caller is already
// compressing and encrypting the next ones, so the CPU and the network overlap.
// Write copies p, so callers may reuse their buffer immediately.
type asyncWriter struct {
	w      io.Writer
	ch     chan []byte
	done   chan struct{} // closed when the goroutine exits
	failed chan struct{} // closed on the first write error
	err    error         // written before failed/done are closed
	once   sync.Once
}

func newAsyncWriter(w io.Writer, depth int) *asyncWriter {
	a := &asyncWriter{
		w:      w,
		ch:     make(chan []byte, depth),
		done:   make(chan struct{}),
		failed: make(chan struct{}),
	}
	go a.loop()
	return a
}

func (a *asyncWriter) loop() {
	defer close(a.done)
	for b := range a.ch {
		if _, err := a.w.Write(b); err != nil {
			a.err = err
			close(a.failed)
			return
		}
	}
}

func (a *asyncWriter) Write(p []byte) (int, error) {
	select {
	case <-a.failed:
		return 0, a.err
	default:
	}
	b := append([]byte(nil), p...)
	select {
	case a.ch <- b:
		return len(p), nil
	case <-a.failed:
		return 0, a.err
	}
}

// Close waits for queued data to be written and returns the first write error.
// It is safe to call more than once.
func (a *asyncWriter) Close() error {
	a.once.Do(func() { close(a.ch) })
	<-a.done
	return a.err
}
