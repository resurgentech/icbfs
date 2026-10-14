package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Log writes timestamped lines to a file (and to w, if set), starting
// the file over once it grows past a size, keeping the previous one.
type Log struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
	echo io.Writer
}

const logLimit = 10 << 20

func openLog(path string, echo io.Writer) (*Log, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	l := &Log{path: path, echo: echo}
	return l, l.reopen()
}

func (l *Log) reopen() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	st, _ := f.Stat()
	l.f, l.size = f, st.Size()
	return nil
}

// Printf logs a line.
func (l *Log) Printf(format string, args ...any) {
	line := time.Now().Format("2006-01-02 15:04:05 ") + fmt.Sprintf(format, args...) + "\n"
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.echo != nil {
		io.WriteString(l.echo, line)
	}
	if l.f == nil {
		return
	}
	if l.size+int64(len(line)) > logLimit {
		l.f.Close()
		os.Rename(l.path, l.path+".1")
		if l.reopen() != nil {
			l.f = nil
			return
		}
	}
	n, _ := l.f.WriteString(line)
	l.size += int64(n)
}

// Close closes the file.
func (l *Log) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
