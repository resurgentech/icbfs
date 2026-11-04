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
	// A stream of unknown length (size -1, StreamPartsUpTo): ended once r
	// has, and at most limit bytes.
	ended bool
	limit int64
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
	if s.size < 0 {
		return p.fillUpTo()
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

// fillUpTo is fill for a stream of unknown length: the part takes what
// r has, up to its most, and is as long as that.
func (p *StreamPart) fillUpTo() error {
	s := p.s
	if int64(cap(s.buf)) < p.n {
		s.buf = make([]byte, p.n)
	}
	n := 0
	if !s.ended {
		var err error
		n, err = io.ReadFull(s.r, s.buf[:p.n])
		switch {
		case err == io.EOF || err == io.ErrUnexpectedEOF:
			s.ended = true
		case err != nil:
			s.err = err
			return err
		case p.last:
			var one [1]byte
			if _, err := io.ReadFull(s.r, one[:]); err == nil {
				s.err = fmt.Errorf("the body is longer than its most, %d bytes", s.limit)
				return s.err
			} else if err != io.EOF {
				s.err = err
				return err
			}
			s.ended = true
		}
	}
	p.n = int64(n)
	s.cur = p.i
	p.rd = bytes.NewReader(s.buf[:n])
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

// StreamPartsUpTo are StreamParts for a reader whose length isn't known,
// only that it's at most most bytes: every part but the last is partSize, and
// r's end, not a size, says which part is last. A store takes each
// part's size from PartSize before it sends it, in order, and stops at
// the first one r ended before; the first part is always sent, empty if
// r is. A part whose bytes r fails to give fails the write, as it does in
// StreamParts; so does r going on past most.
func StreamPartsUpTo(r io.Reader, most, partSize int64) []Part {
	s := &stream{r: r, size: -1, cur: -1, limit: most}
	count := max(1, (most+partSize-1)/partSize)
	parts := make([]Part, count)
	bodies := make([]StreamPart, count)
	for i := range parts {
		n := min(partSize, most-int64(i)*partSize)
		bodies[i] = StreamPart{s: s, i: i, n: n, last: i == len(parts)-1}
		parts[i] = Part{Size: n, Body: &bodies[i]}
	}
	return parts
}

// PartSize is the size of part p of a PutParts write, for a store to
// send it with: its Size; but a part of StreamPartsUpTo is filled first,
// and its size is what r gave, and ok is false for one that r ended
// before, which the store sends no more than any part after it.
func PartSize(p Part) (size int64, ok bool, err error) {
	sp, isStream := p.Body.(*StreamPart)
	if !isStream || sp.s.size >= 0 {
		return p.Size, true, nil
	}
	if err := sp.fill(); err != nil {
		return 0, false, err
	}
	if sp.n == 0 && sp.i > 0 {
		return 0, false, nil
	}
	return sp.n, true, nil
}
