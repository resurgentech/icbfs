package objstore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
)

// StreamParts are the parts of a PutParts write whose bytes arrive from
// one reader of a known length, as they arrive: a download, an HTTP
// upload, a file the caller holds open. Nothing is spooled. The parts
// are filled from r in order, each on its first Read or Seek, into one
// buffer of partSize that they share, so a write holds one part in
// memory at a time; a part is still there, seekable, for the retries of
// its own upload, and gone once the next is taken. A store that uploads
// parts concurrently must upload these one after another (IsStream).
//
// The last part makes sure r ends where size says: a longer body fails
// the part, and so the write, before anything is assembled.
func StreamParts(r io.Reader, size, partSize int64) []Part {
	s := &stream{r: r, size: size, cur: -1}
	var parts []Part
	for off := int64(0); off < size; off += partSize {
		n := min(partSize, size-off)
		i := len(parts)
		parts = append(parts, Part{Size: n, Body: &StreamPart{s: s, i: i, n: n, last: off+n == size}})
	}
	return parts
}

// IsStream reports whether parts came from StreamParts.
func IsStream(parts []Part) bool {
	if len(parts) == 0 {
		return false
	}
	_, ok := parts[0].Body.(*StreamPart)
	return ok
}

type stream struct {
	r    io.Reader
	size int64
	buf  []byte
	cur  int // the part in buf; -1 before the first
	err  error
}

// StreamPart is one of StreamParts: an io.ReadSeeker over its part of the
// stream, filled when first touched.
type StreamPart struct {
	s    *stream
	i    int
	n    int64
	last bool
	rd   *bytes.Reader
}

var errStreamOrder = errors.New("stream part read after the next was taken")

// fill takes the part's bytes from the stream, if it hasn't. Only the part
// after the one in the buffer may be filled.
func (p *StreamPart) fill() error {
	if p.rd != nil {
		return nil
	}
	s := p.s
	if s.err != nil {
		return s.err
	}
	if p.i != s.cur+1 {
		s.err = fmt.Errorf("part %d: %w", p.i, errStreamOrder)
		return s.err
	}
	if int64(cap(s.buf)) < p.n {
		s.buf = make([]byte, p.n)
	}
	buf := s.buf[:p.n]
	if _, err := io.ReadFull(s.r, buf); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			err = fmt.Errorf("the body ended before its %d bytes: %w", s.size, io.ErrUnexpectedEOF)
		}
		s.err = err
		return err
	}
	if p.last {
		var one [1]byte
		if n, _ := s.r.Read(one[:]); n > 0 {
			s.err = fmt.Errorf("the body is longer than its %d bytes", s.size)
			return s.err
		}
	}
	s.cur = p.i
	p.rd = bytes.NewReader(buf)
	return nil
}

func (p *StreamPart) Read(b []byte) (int, error) {
	if err := p.fill(); err != nil {
		return 0, err
	}
	if p.s.cur != p.i {
		return 0, errStreamOrder
	}
	return p.rd.Read(b)
}

func (p *StreamPart) Seek(off int64, whence int) (int64, error) {
	if err := p.fill(); err != nil {
		return 0, err
	}
	if p.s.cur != p.i {
		return 0, errStreamOrder
	}
	return p.rd.Seek(off, whence)
}
