package drive

import (
	"bytes"
	"crypto/md5"
	"crypto/sha256"
	"hash"
	"io"
	"os"
)

// The tiers of a write of unknown length, and of a mirror's spool: up to
// SpoolInMemory bytes in memory; up to SpoolInFile in a temporary file;
// past that, streamed to the destination as the bytes arrive (a
// multipart upload, on icbfs), hashed on the way. Variables, so tests
// can make them small.
var (
	SpoolInMemory int64 = 2 << 20
	SpoolInFile   int64 = 100 << 20
)

// A Spool is a body read to its end before it's written anywhere: in
// memory when small, else in a temporary file, with its size and
// checksums taken as it went by. It's what a write of unknown length
// reads first (icbfsdrive), and what a mirror decides from before it
// writes (the hash it knows, or the bytes). Close removes the file.
//
// A spool with an InFile limit that the body goes past is Streamed: it
// holds the body's first bytes in its file, and Stream reads them and
// then the rest of the body as it arrives; its size and checksums are
// the whole body's once that has been read to its end.
type Spool struct {
	data []byte
	file *os.File
	size int64
	// MD5 and SHA256 are the body's.
	MD5, SHA256 []byte

	rest    io.Reader // a streamed spool's body past the file, through the hashes
	md, sha hash.Hash
	max     int64
}

// SpoolOptions say where and how much a Spool keeps.
type SpoolOptions struct {
	// Dir holds the temporary file; "" is the system's.
	Dir string
	// InMemory is the largest body kept in memory (0: SpoolInMemory).
	InMemory int64
	// InFile, if not 0, is the largest body kept whole in the file: a
	// longer one is streamed (Spool.Streamed). 0 keeps any body whole.
	InFile int64
	// Max is the largest body taken, ErrTooLarge beyond (0: no limit).
	Max int64
	// Hashes are fed the body too, as it goes by: a source's own kind of
	// hash, say.
	Hashes []hash.Hash
}

// NewSpool reads body into a Spool: to its end, unless it's streamed.
func NewSpool(body io.Reader, opts SpoolOptions) (*Spool, error) {
	inMemory, max := opts.InMemory, opts.Max
	if inMemory <= 0 {
		inMemory = SpoolInMemory
	}
	if max <= 0 {
		max = 1<<63 - 1
	}
	md, sha := md5.New(), sha256.New()
	ws := []io.Writer{md, sha}
	for _, h := range opts.Hashes {
		ws = append(ws, h)
	}
	body = io.TeeReader(body, io.MultiWriter(ws...))
	buf := &bytes.Buffer{}
	n, err := io.CopyN(buf, body, inMemory+1)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n <= inMemory {
		if int64(buf.Len()) > max {
			return nil, ErrTooLarge
		}
		return &Spool{data: buf.Bytes(), size: int64(buf.Len()), MD5: md.Sum(nil), SHA256: sha.Sum(nil)}, nil
	}
	inFile := max
	streams := opts.InFile > 0 && opts.InFile < max
	if streams {
		inFile = max64(opts.InFile, n)
	}
	f, err := os.CreateTemp(opts.Dir, "drive-write-")
	if err != nil {
		return nil, err
	}
	total, err := io.Copy(f, io.MultiReader(buf, io.LimitReader(body, inFile-n+1)))
	if err == nil && total > inFile && !streams {
		err = ErrTooLarge
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, err
	}
	if total > inFile {
		return &Spool{file: f, size: total, rest: body, md: md, sha: sha, max: max}, nil
	}
	return &Spool{file: f, size: total, MD5: md.Sum(nil), SHA256: sha.Sum(nil)}, nil
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// Size is the body's length: for a streamed spool, what has gone by so
// far, the whole body's once Stream has been read to its end.
func (s *Spool) Size() int64 { return s.size }

// Bytes is the body when it's in memory; nil when it's in a file.
func (s *Spool) Bytes() []byte { return s.data }

// File is the temporary file holding the body; nil when it's in memory.
// A streamed spool's holds only the body's first bytes.
func (s *Spool) File() *os.File { return s.file }

// Streamed is whether the body was longer than SpoolOptions.InFile, so
// that it's read once, by Stream, as it arrives; and not by ReadAt or
// Reader.
func (s *Spool) Streamed() bool { return s.rest != nil }

// ReadAt reads the body at off, wherever it's kept.
func (s *Spool) ReadAt(p []byte, off int64) (int, error) {
	if s.file != nil {
		return s.file.ReadAt(p, off)
	}
	return bytes.NewReader(s.data).ReadAt(p, off)
}

// Reader reads the body from its start.
func (s *Spool) Reader() io.Reader { return io.NewSectionReader(s, 0, s.size) }

// Stream reads a streamed spool's body, once: what's in the file, then
// the rest as it arrives, past Max being ErrTooLarge. At the body's end,
// with Size, MD5 and SHA256 now the whole body's, it calls end, if not
// nil, and an error end returns is the read's in place of io.EOF: a
// write of the body then fails rather than completes. That's how a
// caller who learns only at the end that the body isn't to be written (a
// checksum it was given, a hash it knows) stops the write.
func (s *Spool) Stream(end func() error) io.Reader {
	return io.MultiReader(io.NewSectionReader(s.file, 0, s.size), &spoolRest{s: s, end: end})
}

// spoolRest is a streamed spool's body past its file.
type spoolRest struct {
	s   *Spool
	end func() error
	err error
}

func (r *spoolRest) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	s := r.s
	n, err := s.rest.Read(p)
	s.size += int64(n)
	if s.size > s.max {
		r.err = ErrTooLarge
		return 0, r.err
	}
	if err == io.EOF {
		s.MD5, s.SHA256 = s.md.Sum(nil), s.sha.Sum(nil)
		r.err = io.EOF
		if r.end != nil {
			if eerr := r.end(); eerr != nil {
				r.err = eerr
			}
		}
		if n > 0 {
			return n, nil
		}
		return 0, r.err
	}
	if err != nil {
		r.err = err
	}
	return n, err
}

// Close removes the temporary file, if there is one.
func (s *Spool) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	os.Remove(s.file.Name())
	s.file = nil
	return err
}
