package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"
)

type Received struct {
	Message  Message
	Snapshot Snapshot
	Err      error
}

func receiveLoop(ctx context.Context, t *Tree, w *Wire) <-chan Received {
	ch := make(chan Received)
	out := make(chan Received)
	go func() {
		ch := ch
		defer close(out)
		var queue []Received
		for ch != nil || len(queue) > 0 {
			var dest chan Received
			var first Received
			if len(queue) > 0 {
				dest = out
				first = queue[0]
			}
			select {
			case r, ok := <-ch:
				if !ok {
					ch = nil
				} else {
					queue = append(queue, r)
				}
			case dest <- first:
				queue[0] = Received{}
				queue = queue[1:]
			case <-ctx.Done():
				for _, r := range queue {
					t.Discard(r.Snapshot)
				}
				return
			}
		}
	}()
	input := ch
	go func() {
		defer close(input)
		for {
			m, err := w.Header()
			s := Snapshot{}
			if err == nil && (m.Type == "ENTRY" || m.Type == "PUSH" || m.Type == "CONFLICT") {
				s, err = t.Receive(w, m)
			} else if err == nil && m.Body != 0 {
				err = errors.New("unexpected body")
			}
			select {
			case input <- Received{m, s, err}:
			case <-ctx.Done():
				t.Discard(s)
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return out
}
func ReadConfig(t *Tree) (Config, error) {
	var c Config
	info, err := t.Root.Lstat(".rcm.yaml")
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if !info.Mode().IsRegular() {
		return c, errors.New(".rcm.yaml must be a regular file")
	}
	b, err := t.Root.ReadFile(".rcm.yaml")
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(strings.NewReader(string(b)))
	d.KnownFields(true)
	err = d.Decode(&c)
	if err == io.EOF {
		err = nil
	}
	if err == nil {
		err = c.Validate()
	}
	return c, err
}
func RunAgent(ctx context.Context, t *Tree, w *Wire) error {
	lock, err := Lock(t)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = t.Recover(); err != nil {
		return err
	}
	hello, err := w.Header()
	if err != nil {
		return err
	}
	if hello.Type != "HELLO" || hello.Version != Protocol || hello.Body != 0 {
		return fmt.Errorf("incompatible protocol: want %d", Protocol)
	}
	project, err := ReadConfig(t)
	if err != nil {
		return err
	}
	c := MergeConfig(project, hello.Config)
	if err = c.Validate(); err != nil {
		return err
	}
	i, err := LoadIgnore(t, c)
	if err != nil {
		return err
	}
	watch, err := NewWatch(t, i, c.Duration())
	if err != nil {
		return err
	}
	defer watch.Close()
	known, ignored, err := t.Scan(i)
	if err != nil {
		return err
	}
	if err = w.Send(Message{Type: "HELLO", Version: Protocol, Config: c, Rules: i.sources}, nil); err != nil {
		return err
	}
	if err = w.Send(Message{Type: "MANIFEST", Entries: known, Ignored: ignored}, nil); err != nil {
		return err
	}
	want, err := w.Header()
	if err != nil {
		return err
	}
	if want.Type != "WANT" || want.Body != 0 {
		return errors.New("expected WANT")
	}
	for _, p := range want.Paths {
		if err = ValidatePath(p); err != nil {
			return err
		}
		if i.Ignored(p, known[p].Kind == "dir") {
			return errors.New("request for ignored path")
		}
		s, err := t.Snapshot(p, true)
		if err != nil {
			return err
		}
		err = t.Send(w, "ENTRY", p, s, Entry{})
		t.Discard(s)
		if err != nil {
			return err
		}
		if s.Entry.Kind == "" {
			delete(known, p)
		} else {
			known[p] = s.Entry
		}
	}
	if err = w.Send(Message{Type: "END"}, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	incoming := receiveLoop(ctx, t, w)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-watch.Errors:
			return err
		case paths := <-watch.Changes:
			if RuleChanged(paths) {
				return errors.New("ignore/config changed; reconnect to reload policy")
			}
			next, err := t.Update(i, known, paths)
			if err != nil {
				return err
			}
			changed := Changes(known, next)
			for _, p := range Ordered(changed, known) {
				s, err := t.Snapshot(p, true)
				if err != nil {
					return err
				}
				if Equal(s.Entry, known[p]) {
					t.Discard(s)
					continue
				}
				err = t.Send(w, "ENTRY", p, s, Entry{})
				t.Discard(s)
				if err != nil {
					return err
				}
				if s.Entry.Kind == "" {
					delete(next, p)
				} else {
					next[p] = s.Entry
				}
			}
			known = next
		case got, ok := <-incoming:
			if !ok {
				return io.EOF
			}
			if got.Err != nil {
				return got.Err
			}
			m, s := got.Message, got.Snapshot
			if m.Type != "PUSH" {
				t.Discard(s)
				return fmt.Errorf("unexpected %s", m.Type)
			}
			if !c.Writable() || GitPath(m.Path) || i.Ignored(m.Path, m.Entry.Kind == "dir") {
				t.Discard(s)
				return fmt.Errorf("push forbidden by policy: %s", m.Path)
			}
			err = t.Apply(m.Path, s, m.Base)
			if err != nil {
				t.Discard(s)
				current, e := t.Snapshot(m.Path, true)
				if e != nil {
					return e
				}
				e = t.Send(w, "CONFLICT", m.Path, current, m.Base)
				t.Discard(current)
				if e != nil {
					return e
				}
			} else {
				if m.Entry.Kind == "" {
					delete(known, m.Path)
				} else {
					known[m.Path] = m.Entry
				}
				t.Discard(s)
				if err = w.Send(Message{Type: "ACK", Path: m.Path, Entry: m.Entry}, nil); err != nil {
					return err
				}
			}
		}
	}
}
