package main

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// Encrypted file layout (integers are big-endian):
//
//	offset  size  field
//	0       8     magic "SSMBAK01" (the last two characters are the format version)
//	8       4     PBKDF2-HMAC-SHA256 iteration count
//	12      4     plaintext bytes per chunk
//	16      16    salt (random, new for every backup)
//	32      7     nonce prefix (random, new for every backup)
//	39      ...   chunks, each AES-256-GCM(plaintext chunk) = ciphertext || 16-byte tag
//
// Chunk i is sealed with nonce = prefix || i (4 bytes) || final-flag (1 byte),
// and the whole 39-byte header is authenticated as additional data. So a chunk
// cannot be reordered, dropped, duplicated, or have the stream truncated or
// extended without the decryption failing. Encrypting in chunks (instead of one
// big AES-GCM call over the whole file) is what lets ssmbak back up many
// gigabytes using a fixed ~1 MiB of memory.
const (
	magic          = "SSMBAK01"
	saltLen        = 16
	noncePrefixLen = 7
	headerLen      = len(magic) + 4 + 4 + saltLen + noncePrefixLen
	tagLen         = 16

	minChunkSize = 4 * 1024
	maxChunkSize = 16 * 1024 * 1024
	maxIter      = 20_000_000
)

// Tunables. Tests lower them to stay fast; both values are stored in the file
// header, so changing the defaults never breaks reading older backups.
var (
	kdfIterations uint32 = 600_000 // OWASP's 2023 minimum for PBKDF2-HMAC-SHA256
	chunkSize     uint32 = 256 * 1024
)

var errAuth = errors.New("decryption failed: wrong password, or the backup is corrupted or was tampered with")

func deriveKey(password string, salt []byte, iterations int) ([]byte, error) {
	return pbkdf2.Key(sha256.New, password, salt, iterations, 32)
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func chunkNonce(prefix []byte, counter uint32, final bool) [12]byte {
	var n [12]byte
	copy(n[:], prefix)
	binary.BigEndian.PutUint32(n[noncePrefixLen:], counter)
	if final {
		n[11] = 1
	}
	return n
}

// encryptWriter encrypts everything written to it and passes the result to w.
// Close must be called to write the final chunk; a stream without it fails to
// decrypt, which is how truncated uploads are detected.
type encryptWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	buf     []byte // plaintext waiting to be sealed, never longer than the chunk size
	out     []byte // reused for sealed chunks
	counter uint32
	closed  bool
	err     error
}

func newEncryptWriter(w io.Writer, password string) (*encryptWriter, error) {
	hdr := make([]byte, headerLen)
	copy(hdr, magic)
	binary.BigEndian.PutUint32(hdr[8:], kdfIterations)
	binary.BigEndian.PutUint32(hdr[12:], chunkSize)
	if _, err := io.ReadFull(rand.Reader, hdr[16:]); err != nil { // salt + nonce prefix
		return nil, err
	}
	key, err := deriveKey(password, hdr[16:32], int(kdfIterations))
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(hdr); err != nil {
		return nil, err
	}
	return &encryptWriter{
		w:      w,
		aead:   aead,
		header: hdr,
		prefix: hdr[32:headerLen],
		buf:    make([]byte, 0, chunkSize),
		out:    make([]byte, 0, int(chunkSize)+tagLen),
	}, nil
}

func (e *encryptWriter) Write(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	if e.closed {
		return 0, errors.New("ssmbak: write after close")
	}
	written := 0
	for len(p) > 0 {
		if len(e.buf) == cap(e.buf) {
			// The buffer is full and more data is arriving, so it is not the last chunk.
			if err := e.flush(false); err != nil {
				return written, err
			}
		}
		n := copy(e.buf[len(e.buf):cap(e.buf)], p)
		e.buf = e.buf[:len(e.buf)+n]
		p = p[n:]
		written += n
	}
	return written, nil
}

func (e *encryptWriter) flush(final bool) error {
	if e.counter == math.MaxUint32 {
		e.err = errors.New("archive too large for this format")
		return e.err
	}
	nonce := chunkNonce(e.prefix, e.counter, final)
	e.out = e.aead.Seal(e.out[:0], nonce[:], e.buf, e.header)
	if _, err := e.w.Write(e.out); err != nil {
		e.err = err
		return err
	}
	e.buf = e.buf[:0]
	e.counter++
	return nil
}

// Close seals the final chunk. It does not close the underlying writer.
func (e *encryptWriter) Close() error {
	if e.closed {
		return e.err
	}
	e.closed = true
	if e.err != nil {
		return e.err
	}
	return e.flush(true)
}

// decryptReader is the inverse of encryptWriter. It only returns plaintext that
// has been authenticated, and it returns io.EOF only after it has verified the
// final chunk.
type decryptReader struct {
	r       *bufio.Reader
	aead    cipher.AEAD
	header  []byte
	prefix  []byte
	in      []byte // one ciphertext chunk
	plain   []byte // decrypted bytes not yet returned
	pbuf    []byte
	counter uint32
	done    bool
	err     error
}

func newDecryptReader(r io.Reader, password string) (*decryptReader, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	hdr := make([]byte, headerLen)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return nil, errors.New("not an ssmbak backup (file is too short)")
	}
	if string(hdr[:6]) != magic[:6] {
		return nil, errors.New("not an ssmbak backup (bad magic bytes)")
	}
	if string(hdr[:8]) != magic {
		return nil, fmt.Errorf("unsupported backup format version %q (this build reads %q)", hdr[6:8], magic[6:8])
	}
	iterations := binary.BigEndian.Uint32(hdr[8:])
	chunk := binary.BigEndian.Uint32(hdr[12:])
	if iterations == 0 || iterations > maxIter {
		return nil, fmt.Errorf("corrupt header: implausible KDF iteration count %d", iterations)
	}
	if chunk < minChunkSize || chunk > maxChunkSize {
		return nil, fmt.Errorf("corrupt header: implausible chunk size %d", chunk)
	}
	key, err := deriveKey(password, hdr[16:32], int(iterations))
	if err != nil {
		return nil, err
	}
	aead, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	return &decryptReader{
		r:      br,
		aead:   aead,
		header: hdr,
		prefix: hdr[32:headerLen],
		in:     make([]byte, int(chunk)+tagLen),
		pbuf:   make([]byte, 0, chunk),
	}, nil
}

func (d *decryptReader) Read(p []byte) (int, error) {
	for len(d.plain) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.done {
			return 0, io.EOF
		}
		if err := d.next(); err != nil {
			d.err = err
			return 0, err
		}
	}
	n := copy(p, d.plain)
	d.plain = d.plain[n:]
	return n, nil
}

func (d *decryptReader) next() error {
	n, err := io.ReadFull(d.r, d.in)
	final := false
	switch {
	case err == nil:
		// A full chunk is the last one only if nothing follows it.
		if _, perr := d.r.Peek(1); perr == io.EOF {
			final = true
		} else if perr != nil {
			return perr
		}
	case errors.Is(err, io.ErrUnexpectedEOF):
		final = true
	case errors.Is(err, io.EOF):
		return errors.New("backup is truncated (the final chunk is missing)")
	default:
		return err
	}
	if n < tagLen {
		return errors.New("backup is truncated (incomplete final chunk)")
	}
	nonce := chunkNonce(d.prefix, d.counter, final)
	pt, err := d.aead.Open(d.pbuf[:0], nonce[:], d.in[:n], d.header)
	if err != nil {
		return errAuth
	}
	d.pbuf = pt
	d.plain = pt
	d.counter++
	d.done = final
	return nil
}
