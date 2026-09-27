package mirror

import (
	"io"
	"sync"
)

var copyBuffers = sync.Pool{New: func() any { return make([]byte, 128<<10) }}

type readerOnly struct{ io.Reader }

// Reuse buffers across thousands of small source files. Wrapping the reader
// prevents os.File.WriteTo from allocating a different buffer for each file.
func copyStream(w io.Writer, r io.Reader) (int64, error) {
	b := copyBuffers.Get().([]byte)
	defer copyBuffers.Put(b)
	return io.CopyBuffer(w, readerOnly{r}, b)
}
func copyExactly(w io.Writer, r io.Reader, n int64) (int64, error) {
	copied, err := copyStream(w, io.LimitReader(r, n))
	if err == nil && copied != n {
		err = io.ErrUnexpectedEOF
	}
	return copied, err
}
