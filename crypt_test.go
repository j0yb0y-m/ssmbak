package main

import (
	"bytes"
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

// lowCost makes encryption cheap for tests. The values live in the file
// header, so decryption works regardless of the global defaults.
func lowCost(t *testing.T) {
	t.Helper()
	oi, oc := kdfIterations, chunkSize
	kdfIterations, chunkSize = 1000, minChunkSize
	t.Cleanup(func() { kdfIterations, chunkSize = oi, oc })
}

func randomBytes(n int) []byte {
	r := rand.New(rand.NewPCG(uint64(n), 42))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.UintN(256))
	}
	return b
}

func encryptBytes(t *testing.T, password string, data []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := newEncryptWriter(&out, password)
	if err != nil {
		t.Fatal(err)
	}
	for rest := data; len(rest) > 0; { // odd-sized writes exercise the buffering
		n := min(1000, len(rest))
		if _, err := w.Write(rest[:n]); err != nil {
			t.Fatal(err)
		}
		rest = rest[n:]
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func decryptBytes(enc []byte, password string) ([]byte, error) {
	r, err := newDecryptReader(bytes.NewReader(enc), password)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(r)
}

func TestEncryptRoundTrip(t *testing.T) {
	lowCost(t)
	c := int(chunkSize)
	for _, n := range []int{0, 1, c - 1, c, c + 1, 2 * c, 3*c + 17, 100_000} {
		data := randomBytes(n)
		enc := encryptBytes(t, "pw", data)
		got, err := decryptBytes(enc, "pw")
		if err != nil {
			t.Fatalf("size %d: %v", n, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("size %d: round trip mismatch", n)
		}
	}
}

func TestEncryptionIsRandomised(t *testing.T) {
	lowCost(t)
	a := encryptBytes(t, "pw", []byte("same input"))
	b := encryptBytes(t, "pw", []byte("same input"))
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of the same data are identical (salt/nonce not random)")
	}
}

func TestWrongPassword(t *testing.T) {
	lowCost(t)
	enc := encryptBytes(t, "right", randomBytes(10_000))
	if _, err := decryptBytes(enc, "wrong"); !errors.Is(err, errAuth) {
		t.Fatalf("want errAuth, got %v", err)
	}
}

func TestTamperingAndTruncationAreDetected(t *testing.T) {
	lowCost(t)
	c := int(chunkSize) + tagLen
	data := randomBytes(3*int(chunkSize) + 100) // 4 chunks
	enc := encryptBytes(t, "pw", data)

	mutate := func(name string, f func([]byte) []byte) {
		t.Helper()
		cp := append([]byte(nil), enc...)
		if _, err := decryptBytes(f(cp), "pw"); err == nil {
			t.Errorf("%s: corruption was not detected", name)
		}
	}
	for _, off := range []int{20, 35, headerLen + 5, headerLen + c + 7, len(enc) - 1} {
		off := off
		mutate("bit flip", func(b []byte) []byte { b[off] ^= 0x01; return b })
	}
	mutate("truncate at chunk boundary", func(b []byte) []byte { return b[:headerLen+2*c] })
	mutate("truncate mid chunk", func(b []byte) []byte { return b[:headerLen+c+50] })
	mutate("drop last byte", func(b []byte) []byte { return b[:len(b)-1] })
	mutate("append byte", func(b []byte) []byte { return append(b, 0) })
	mutate("header only", func(b []byte) []byte { return b[:headerLen] })
	mutate("swap chunks", func(b []byte) []byte {
		x := append([]byte(nil), b[headerLen:headerLen+c]...)
		copy(b[headerLen:], b[headerLen+c:headerLen+2*c])
		copy(b[headerLen+c:], x)
		return b
	})
	mutate("duplicate chunk", func(b []byte) []byte {
		out := append([]byte(nil), b[:headerLen+c]...)
		return append(out, b[headerLen:]...)
	})
}

func TestNotAnSsmbakFile(t *testing.T) {
	if _, err := decryptBytes([]byte("this is just some text file, definitely not a backup"), "pw"); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := decryptBytes(nil, "pw"); err == nil {
		t.Fatal("expected an error for empty input")
	}
}
