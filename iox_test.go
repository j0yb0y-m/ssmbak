package main

import (
	"bytes"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestAsyncWriterKeepsOrder(t *testing.T) {
	var sink bytes.Buffer
	aw := newAsyncWriter(&sink, 4)
	var want bytes.Buffer
	for i := 0; i < 500; i++ {
		chunk := bytes.Repeat([]byte{byte(i)}, i%97+1)
		want.Write(chunk)
		if _, err := aw.Write(chunk); err != nil {
			t.Fatal(err)
		}
		chunk[0] ^= 0xff // the caller may reuse its buffer; the writer must have copied it
	}
	if err := aw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := aw.Close(); err != nil {
		t.Fatal("second Close must be harmless:", err)
	}
	if !bytes.Equal(sink.Bytes(), want.Bytes()) {
		t.Fatal("output differs from input")
	}
}

type failingWriter struct {
	after int
	n     int
}

func (f *failingWriter) Write(p []byte) (int, error) {
	f.n += len(p)
	if f.n > f.after {
		return 0, errors.New("disk on fire")
	}
	return len(p), nil
}

func TestAsyncWriterReportsErrorsWithoutDeadlock(t *testing.T) {
	aw := newAsyncWriter(&failingWriter{after: 1000}, 2)
	done := make(chan error, 1)
	go func() {
		var err error
		for i := 0; i < 10_000 && err == nil; i++ {
			_, err = aw.Write(make([]byte, 100))
		}
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("writer never reported the error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deadlock: Write blocked after the destination failed")
	}
	if err := aw.Close(); err == nil {
		t.Fatal("Close should return the write error")
	}
}

func TestCountWriter(t *testing.T) {
	var n atomic.Int64
	var sink bytes.Buffer
	cw := &countWriter{w: &sink, n: &n}
	cw.Write([]byte("hello"))
	cw.Write([]byte(" world"))
	if n.Load() != 11 {
		t.Fatalf("count = %d", n.Load())
	}
}
