package query

import (
	"bytes"
	"errors"
	"io"
)

// revChunk is how much a reverseReader reads per step.
const revChunk = 64 << 10

// reverseReader returns the complete lines of f that end at or before bound,
// last line first. A bound that is not after a newline (the end of a file the
// collector is still writing) is moved back to the last complete line.
type reverseReader struct {
	f         io.ReaderAt
	data      []byte // bytes [dataStart, dataStart+len(data)); ends with '\n' unless empty
	dataStart int64
	read      int64 // bytes of the returned lines, for scan limits
	trimmed   bool  // the unterminated tail has been dropped
}

func newReverseReader(f io.ReaderAt, bound int64) *reverseReader {
	return &reverseReader{f: f, dataStart: bound}
}

// load prepends up to revChunk bytes before dataStart.
func (r *reverseReader) load() error {
	n := min(int64(revChunk), r.dataStart)
	if len(r.data) > maxLineBytes*4 {
		return errors.New("line too long")
	}
	buf := make([]byte, int(n)+len(r.data))
	if _, err := r.f.ReadAt(buf[:n], r.dataStart-n); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	copy(buf[n:], r.data)
	r.data = buf
	r.dataStart -= n
	return nil
}

// prev returns the previous line (without newline) and its offset, or io.EOF.
func (r *reverseReader) prev() ([]byte, int64, error) {
	for !r.trimmed {
		if i := bytes.LastIndexByte(r.data, '\n'); i >= 0 {
			r.data = r.data[:i+1]
			r.trimmed = true
			break
		}
		if r.dataStart == 0 {
			return nil, 0, io.EOF // no complete line at all
		}
		if err := r.load(); err != nil {
			return nil, 0, err
		}
	}
	for {
		if len(r.data) == 0 && r.dataStart == 0 {
			return nil, 0, io.EOF
		}
		if len(r.data) > 0 {
			body := r.data[:len(r.data)-1]
			if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
				line := body[i+1:]
				r.data = r.data[:i+1]
				r.read += int64(len(line) + 1)
				return line, r.dataStart + int64(i+1), nil
			}
			if r.dataStart == 0 {
				r.data = r.data[:0]
				r.read += int64(len(body) + 1)
				return body, 0, nil
			}
		}
		if err := r.load(); err != nil {
			return nil, 0, err
		}
	}
}
