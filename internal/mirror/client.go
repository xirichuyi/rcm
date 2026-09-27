package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"
)

type Client struct {
	Tree                    *Tree
	State                   *State
	Config                  Config
	Log                     func(string, ...any)
	Status                  func(string)
	pending                 map[string]Entry
	remote                  Manifest
	wire                    *Wire
	dirty                   bool
	Verbose                 bool
	remoteCount, localCount int
	remoteLast, localLast   string
}

func (c *Client) log(f string, a ...any) {
	if c.Log != nil {
		c.Log(f, a...)
	}
}
func (c *Client) status(s string) {
	if c.Status != nil {
		c.Status(s)
	}
}
func (c *Client) Session(ctx context.Context, w *Wire) error {
	c.wire = w
	defer c.flushEvents()
	c.pending = map[string]Entry{}
	c.remote = Manifest{}
	if c.State == nil {
		var err error
		c.State, err = LoadState(c.Tree)
		if err != nil {
			return err
		}
	}
	defer func() {
		if c.dirty {
			if err := c.State.Save(c.Tree); err != nil {
				c.log("baseline persistence failed: %v", err)
			}
		}
	}()
	c.status("Reconciling")
	if err := w.Send(Message{Type: "HELLO", Version: Protocol, Config: c.Config}, nil); err != nil {
		return err
	}
	h, err := w.Header()
	if err != nil {
		return err
	}
	if h.Type != "HELLO" || h.Version != Protocol || h.Body != 0 {
		return errors.New("incompatible agent protocol")
	}
	c.Config = h.Config
	i := &Ignore{includeGit: c.Config.IncludeGit}
	for _, r := range h.Rules {
		i.Add(r.Base, r.Lines)
	}
	watch, err := NewWatch(c.Tree, i, c.Config.Duration())
	if err != nil {
		return err
	}
	defer watch.Close()
	m, err := w.Header()
	if err != nil {
		return err
	}
	if m.Type != "MANIFEST" || m.Body != 0 {
		return errors.New("expected MANIFEST")
	}
	for p, e := range m.Entries {
		if err = ValidateEntry(p, e); err != nil {
			return err
		}
		if e.Kind == "" {
			return errors.New("manifest tombstone")
		}
		c.remote[p] = e
	}
	local, _, err := c.Tree.Scan(i)
	if err != nil {
		return err
	}
	wanted := Manifest{}
	for p, e := range c.remote {
		if !Equal(local[p], e) || c.State.Conflicts[p].Path != "" {
			wanted[p] = e
		} else {
			c.State.SetBase(p, e)
			c.dirty = true
		}
	}
	paths := Ordered(wanted, nil)
	if err = w.Send(Message{Type: "WANT", Paths: paths}, nil); err != nil {
		return err
	}
	// Remote tombstones are computed from the complete manifest, never from a lost event.
	deleted := Manifest{}
	for p, e := range c.State.Base {
		if _, ok := c.remote[p]; !ok && !i.Ignored(p, e.Kind == "dir") {
			deleted[p] = Entry{}
		}
	}
	for p, cf := range c.State.Conflicts {
		if _, ok := c.remote[p]; !ok && !i.Ignored(p, cf.Local.Entry.Kind == "dir") {
			deleted[p] = Entry{}
		}
	}
	for _, p := range Ordered(deleted, nil) {
		if err = c.process(p, Snapshot{}); err != nil {
			return err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	incoming := receiveLoop(ctx, c.Tree, w)
	initial := true
	var transferred int64
	controls := time.NewTicker(500 * time.Millisecond)
	defer controls.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-watch.Errors:
			return err
		case got, ok := <-incoming:
			if !ok {
				return io.EOF
			}
			if got.Err != nil {
				return got.Err
			}
			msg := got.Message
			transferred += msg.Body
			switch msg.Type {
			case "ENTRY":
				if err = c.process(msg.Path, got.Snapshot); err != nil {
					return err
				}
			case "CONFLICT":
				delete(c.pending, msg.Path)
				c.remote[msg.Path] = msg.Entry
				if err = c.State.Conflict(c.Tree, msg.Path, got.Snapshot, "remote changed before upload"); err != nil {
					return err
				}
				c.log("CONFLICT %s", msg.Path)
			case "ACK":
				pending, ok := c.pending[msg.Path]
				if !ok || !Equal(pending, msg.Entry) {
					return errors.New("unexpected ACK")
				}
				delete(c.pending, msg.Path)
				c.remote[msg.Path] = msg.Entry
				c.State.SetBase(msg.Path, msg.Entry)
				c.dirty = true
			case "END":
				if !initial {
					return errors.New("duplicate END")
				}
				initial = false
				if err = c.State.Save(c.Tree); err != nil {
					return err
				}
				c.dirty = false

			default:
				return fmt.Errorf("unexpected frame %s", msg.Type)
			}
			if !initial {
				if msg.Path != "" {
					if err = c.pushChanged([]string{msg.Path}, i); err != nil {
						return err
					}
				}
				if msg.Type == "END" {
					if err = c.pushChanged([]string{"."}, i); err != nil {
						return err
					}
					c.status("Watching")
					c.log("Initial sync complete: %d entries; transferred %.2f MiB; ignored %d entries/subtrees; watching", len(c.remote), float64(transferred)/(1<<20), m.Ignored)
				}
			}
		case paths := <-watch.Changes:
			if !initial {
				if err = c.pushChanged(paths, i); err != nil {
					return err
				}
			}
		case <-controls.C:
			c.flushEvents()
			if c.dirty {
				if err = c.State.Save(c.Tree); err != nil {
					return err
				}
				c.dirty = false
			}
			if !initial {
				if err = c.resolutions(); err != nil {
					return err
				}
			}
		}
	}
}
func (c *Client) process(p string, s Snapshot) error {
	if s.Entry.Kind == "" {
		delete(c.remote, p)
	} else {
		c.remote[p] = s.Entry
	}
	local, err := c.Tree.Snapshot(p, false)
	if err != nil {
		// A parent changed into a file/symlink: retain the incoming version and flag it.
		c.State.Conflicts[p] = Conflict{Path: p, Base: c.State.Base[p], Remote: s, Reason: err.Error(), Updated: time.Now()}
		return c.State.Save(c.Tree)
	}
	base := c.State.Base[p]
	if Equal(local.Entry, s.Entry) {
		c.State.SetBase(p, s.Entry)
		c.Tree.Discard(s)
		c.dirty = true
		return nil
	}
	if _, exists := c.State.Conflicts[p]; exists {
		return c.State.Conflict(c.Tree, p, s, "unresolved divergent versions")
	}
	if _, inflight := c.pending[p]; inflight {
		return c.State.Conflict(c.Tree, p, s, "remote update while upload pending")
	}
	if Equal(local.Entry, base) {
		err = c.Tree.Apply(p, s, local.Entry)
		if err == nil {
			c.State.SetBase(p, s.Entry)
			c.dirty = true
			c.Tree.Discard(s)
			c.event(false, op(base, s.Entry), p)
			return nil
		}
		if !errors.Is(err, ErrConflict) {
			return err
		}
		if err = c.State.Conflict(c.Tree, p, s, err.Error()); err != nil {
			return err
		}
		c.log("CONFLICT %s", p)
		return nil
	}
	if Equal(s.Entry, base) {
		c.Tree.Discard(s)
		return nil
	}
	if err = c.State.Conflict(c.Tree, p, s, "both local and remote changed"); err != nil {
		return err
	}
	c.log("CONFLICT %s", p)
	return nil
}
func op(before, after Entry) string {
	if after.Kind == "" {
		return "D"
	}
	if before.Kind == "" {
		return "+"
	}
	return "M"
}
func (c *Client) pushChanged(paths []string, i *Ignore) error {
	if !c.Config.Writable() {
		return nil
	}
	// Use baseline as an index. Only affected subtrees are rehashed on ordinary events.
	next, err := c.Tree.Update(i, c.State.Base, paths)
	if err != nil {
		return err
	}
	changes := Changes(c.State.Base, next)
	for _, p := range Ordered(changes, c.State.Base) {
		if GitPath(p) || i.Ignored(p, next[p].Kind == "dir") {
			continue
		}
		if _, ok := c.pending[p]; ok {
			continue
		}
		if _, ok := c.State.Conflicts[p]; ok {
			continue
		}
		s, err := c.Tree.Snapshot(p, true)
		if err != nil {
			return err
		}
		if Equal(s.Entry, c.State.Base[p]) {
			c.Tree.Discard(s)
			continue
		}
		c.pending[p] = s.Entry
		err = c.Tree.Send(c.wire, "PUSH", p, s, c.State.Base[p])
		c.Tree.Discard(s)
		if err != nil {
			return err
		}
		c.event(true, op(c.State.Base[p], s.Entry), p)
	}
	return nil
}
func (c *Client) resolutions() error {
	f, err := c.Tree.Root.Open(Internal + "/requests")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ds, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, d := range ds {
		if !d.Type().IsRegular() {
			continue
		}
		name := path.Join(Internal, "requests", d.Name())
		b, err := c.Tree.Root.ReadFile(name)
		if err != nil {
			return err
		}
		var r Resolution
		if err = json.Unmarshal(b, &r); err != nil {
			return err
		}
		if err = ValidatePath(r.Path); err != nil {
			return err
		}
		if _, inflight := c.pending[r.Path]; inflight {
			continue
		}
		cf, ok := c.State.Conflicts[r.Path]
		if !ok {
			_ = c.Tree.Root.Remove(name)
			continue
		}
		cur, err := c.Tree.Snapshot(r.Path, true)
		if err != nil {
			return err
		}
		if !Equal(r.Local, cur.Entry) || !Equal(r.Remote, cf.Remote.Entry) {
			c.Tree.Discard(cur)
			c.log("Resolution rejected: %s changed again", r.Path)
			_ = c.Tree.Root.Remove(name)
			continue
		}
		switch r.Choice {
		case "remote":
			candidate, cloneErr := c.Tree.Clone(cf.Remote)
			if cloneErr != nil {
				return cloneErr
			}
			err = c.Tree.Apply(r.Path, candidate, cur.Entry)
			c.Tree.Discard(candidate)
			if err == nil {
				c.State.SetBase(r.Path, cf.Remote.Entry)
				err = c.State.Save(c.Tree)
			}
		case "local":
			if !c.Config.Writable() || GitPath(r.Path) {
				c.log("Local resolution forbidden by policy: %s", r.Path)
			} else {
				c.pending[r.Path] = cur.Entry
				err = c.Tree.Send(c.wire, "PUSH", r.Path, cur, cf.Remote.Entry)
			}
		default:
			err = errors.New("invalid resolution")
		}
		c.Tree.Discard(cur)
		if err != nil {
			if !errors.Is(err, ErrConflict) {
				return err
			}
			c.log("Resolution rejected: %s: %v", r.Path, err)
		} else {
			c.log("Resolution requested: %s (%s)", r.Path, r.Choice)
		}
		if err = c.Tree.Root.Remove(name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Client) event(upload bool, operation, p string) {
	direction := "Remote → Local"
	if upload {
		direction = "Local → Remote"
	}
	if c.Verbose {
		c.log("%s %s %s", direction, operation, p)
		return
	}
	if upload {
		c.localCount++
		c.localLast = p
	} else {
		c.remoteCount++
		c.remoteLast = p
	}
}
func (c *Client) flushEvents() {
	if c.remoteCount > 0 {
		c.log("Remote → Local: %d changes (last: %s)", c.remoteCount, c.remoteLast)
		c.remoteCount = 0
	}
	if c.localCount > 0 {
		c.log("Local → Remote: %d uploads submitted (last: %s)", c.localCount, c.localLast)
		c.localCount = 0
	}
}
