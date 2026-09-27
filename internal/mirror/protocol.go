package mirror

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

const MaxHeader = 32 << 20
const MaxBody int64 = 1 << 40

type Message struct {
	Ignored    int          `json:"ignored,omitempty"`
	Type       string       `json:"type"`
	Version    int          `json:"version,omitempty"`
	Generation uint64       `json:"generation,omitempty"`
	Path       string       `json:"path,omitempty"`
	Entry      Entry        `json:"entry,omitempty"`
	Base       Entry        `json:"base,omitempty"`
	Entries    Manifest     `json:"entries,omitempty"`
	Paths      []string     `json:"paths,omitempty"`
	Body       int64        `json:"body,omitempty"`
	Error      string       `json:"error,omitempty"`
	Config     Config       `json:"config,omitempty"`
	Rules      []IgnoreRule `json:"rules,omitempty"`
}
type Wire struct {
	r          io.Reader
	w          io.Writer
	mu         sync.Mutex
	generation uint64
}

func NewWire(r io.Reader, w io.Writer) *Wire { return &Wire{r: r, w: w} }
func (w *Wire) Send(m Message, body io.Reader) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.generation++
	m.Generation = w.generation
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if len(b) > MaxHeader || m.Body < 0 || m.Body > MaxBody {
		return fmt.Errorf("frame exceeds limits")
	}
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(b)))
	if err = writeAll(w.w, n[:]); err != nil {
		return err
	}
	if err = writeAll(w.w, b); err != nil {
		return err
	}
	if m.Body > 0 {
		if body == nil {
			return fmt.Errorf("missing frame body")
		}
		_, err = copyExactly(w.w, body, m.Body)
	}
	return err
}
func (w *Wire) Header() (Message, error) {
	var n [4]byte
	var m Message
	if _, err := io.ReadFull(w.r, n[:]); err != nil {
		return m, err
	}
	size := binary.BigEndian.Uint32(n[:])
	if size == 0 || size > MaxHeader {
		return m, fmt.Errorf("invalid header size %d", size)
	}
	b := make([]byte, size)
	if _, err := io.ReadFull(w.r, b); err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, err
	}
	if m.Body < 0 || m.Body > MaxBody {
		return m, fmt.Errorf("invalid body size")
	}
	if m.Body > 0 && (m.Entry.Kind != "file" || m.Entry.Size != m.Body) {
		return m, fmt.Errorf("inconsistent body")
	}
	return m, nil
}
func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
