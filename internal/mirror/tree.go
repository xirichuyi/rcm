package mirror

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Tree struct {
	Root *os.Root
	Dir  string
}
type Snapshot struct {
	Entry  Entry
	Blob   string
	synced bool
}

func ID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func OpenTree(dir string) (*Tree, error) {
	r, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	t := &Tree{Root: r, Dir: dir}
	for _, p := range []string{Internal, Internal + "/spool", Internal + "/history"} {
		info, e := r.Lstat(p)
		if e == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				r.Close()
				return nil, fmt.Errorf("unsafe private directory %s", p)
			}
			if info.Mode().Perm()&0077 != 0 {
				r.Close()
				return nil, fmt.Errorf("private directory %s must have mode 0700", p)
			}
		} else if errors.Is(e, fs.ErrNotExist) {
			if e = r.Mkdir(p, 0700); e != nil {
				r.Close()
				return nil, e
			}
		} else {
			r.Close()
			return nil, e
		}
	}
	return t, nil
}
func (t *Tree) Close() error { return t.Root.Close() }
func (t *Tree) Parents(p string) error {
	if err := ValidatePath(p); err != nil {
		return err
	}
	parts := strings.Split(p, "/")
	for n := 1; n < len(parts); n++ {
		info, err := t.Root.Lstat(strings.Join(parts[:n], "/"))
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe/non-directory parent of %s", p)
		}
	}
	return nil
}
func (t *Tree) snapshot(p string, body, internal bool) (Snapshot, error) {
	var s Snapshot
	if !internal {
		if err := t.Parents(p); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return s, nil
			}
			return s, err
		}
	}
	info, err := t.Root.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	e := Entry{Mtime: info.ModTime().UnixNano()}
	switch {
	case info.IsDir():
		e.Kind = "dir"
		e.Mode = 0755
	case info.Mode()&os.ModeSymlink != 0:
		target, err := t.Root.Readlink(p)
		if err != nil {
			return s, err
		}
		e.Kind = "link"
		e.Target = target
		e.Hash = Digest([]byte(target))
	case info.Mode().IsRegular():
		f, err := t.Root.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		if err != nil {
			return s, err
		}
		defer f.Close()
		before, err := f.Stat()
		if err != nil {
			return s, err
		}
		if !before.Mode().IsRegular() {
			return s, fmt.Errorf("not a regular file: %s", p)
		}
		h := sha256.New()
		var dst io.Writer = h
		var out *os.File
		if body {
			s.Blob = Internal + "/spool/" + ID()
			perm := os.FileMode(0644)
			if before.Mode()&0111 != 0 {
				perm = 0755
			}
			out, err = t.Root.OpenFile(s.Blob, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
			if err != nil {
				return s, err
			}
			dst = io.MultiWriter(h, out)
		}
		size, err := copyStream(dst, f)
		if out != nil {
			closeErr := out.Close()
			if err == nil {
				err = closeErr
			}
		}
		after, statErr := f.Stat()
		if err == nil {
			err = statErr
		}
		if err == nil && (before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) || size != after.Size()) {
			err = fmt.Errorf("%w: %s", ErrUnstable, p)
		}
		if err != nil {
			t.Discard(s)
			return Snapshot{}, err
		}
		e.Kind = "file"
		e.Size = size
		e.Hash = hex.EncodeToString(h.Sum(nil))
		e.Mode = 0644
		if before.Mode()&0111 != 0 {
			e.Mode = 0755
		}
		e.Mtime = after.ModTime().UnixNano()
	default:
		return s, nil
	}
	s.Entry = e
	if !internal {
		if err := ValidateEntry(p, e); err != nil {
			t.Discard(s)
			return Snapshot{}, err
		}
	}
	return s, nil
}
func (t *Tree) Snapshot(p string, body bool) (Snapshot, error) {
	var s Snapshot
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		s, err = t.snapshot(p, body, false)
		if !errors.Is(err, ErrUnstable) && !errors.Is(err, fs.ErrNotExist) {
			break
		}
	}
	return s, err
}
func (t *Tree) Discard(s Snapshot) {
	if s.Blob != "" {
		_ = t.Root.Remove(s.Blob)
	}
}
func (t *Tree) Scan(i *Ignore) (Manifest, int, error) {
	m := Manifest{}
	ignored := 0
	var walk func(string) error
	walk = func(dir string) error {
		f, err := t.Root.Open(dir)
		if err != nil {
			return err
		}
		ds, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, d := range ds {
			p := path.Join(dir, d.Name())
			if i.Match(p, d.IsDir()) {
				ignored++
				continue
			}
			s, err := t.Snapshot(p, false)
			if err != nil {
				return err
			}
			if s.Entry.Kind == "" {
				continue
			}
			m[p] = s.Entry
			if s.Entry.Kind == "dir" {
				if err := walk(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	err := walk(".")
	return m, ignored, err
}
func (t *Tree) Receive(w *Wire, m Message) (Snapshot, error) {
	s := Snapshot{Entry: m.Entry}
	if err := ValidateEntry(m.Path, m.Entry); err != nil {
		return s, err
	}
	want := int64(0)
	if m.Entry.Kind == "file" {
		want = m.Entry.Size
	}
	if want != m.Body {
		return s, errors.New("body length mismatch")
	}
	if m.Entry.Kind != "file" {
		s.synced = true
		return s, nil
	}
	s.Blob = Internal + "/spool/" + ID()
	f, err := t.Root.OpenFile(s.Blob, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(m.Entry.Mode))
	if err != nil {
		return s, err
	}
	h := sha256.New()
	_, err = copyExactly(io.MultiWriter(f, h), w.r, m.Body)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && hex.EncodeToString(h.Sum(nil)) != m.Entry.Hash {
		err = errors.New("body checksum mismatch")
	}
	if err != nil {
		t.Discard(s)
		return Snapshot{}, err
	}
	s.synced = true
	return s, nil
}
func (t *Tree) Send(w *Wire, typ, p string, s Snapshot, base Entry) error {
	m := Message{Type: typ, Path: p, Entry: s.Entry, Base: base}
	if s.Entry.Kind != "file" {
		return w.Send(m, nil)
	}
	f, err := t.Root.Open(s.Blob)
	if err != nil {
		return err
	}
	defer f.Close()
	m.Body = s.Entry.Size
	return w.Send(m, f)
}

// Apply uses a recoverable no-clobber transaction. The old inode is moved to
// history before validation, so a concurrent replacement is captured, not lost.
// Readers can observe a brief ENOENT, but never a partially written file.
func (t *Tree) Apply(p string, s Snapshot, expected Entry) error {
	if err := ValidateEntry(p, s.Entry); err != nil {
		return err
	}
	if err := t.Parents(p); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	cur, err := t.Snapshot(p, false)
	if err != nil {
		return err
	}
	if !Equal(cur.Entry, expected) {
		return ErrConflict
	}
	if Equal(cur.Entry, s.Entry) {
		return nil
	}
	if cur.Entry.Kind == "dir" {
		if s.Entry.Kind == "dir" {
			return nil
		}
		if err = t.removeEmptyDirectory(p); err != nil {
			return fmt.Errorf("%w: nonempty directory %s", ErrConflict, p)
		}
	} else if cur.Entry.Kind != "" {
		backup := Internal + "/history/" + ID()
		record := recovery{p, expected, s.Entry, backup, time.Now()}
		if err = t.WriteJSON(backup+".json", record); err != nil {
			return err
		}
		if err = t.Root.Rename(p, backup); err != nil {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		_ = t.syncDir(path.Dir(p))
		_ = t.syncDir(Internal + "/history")
		moved, e := t.snapshot(backup, false, true)
		if e != nil || !Equal(moved.Entry, expected) {
			_ = t.restore(backup, p)
			_ = t.finishRecovery(backup)
			return fmt.Errorf("%w: captured competing edit, saved at %s", ErrConflict, backup)
		}
		if err = t.install(p, s); err != nil {
			_ = t.restore(backup, p)
			_ = t.finishRecovery(backup)
			return fmt.Errorf("%w: %v (previous version: %s)", ErrConflict, err, backup)
		}
		if err = t.syncDir(path.Dir(p)); err != nil {
			return err
		}
		return t.finishRecovery(backup)
	}
	if err = t.install(p, s); err != nil {
		return fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return t.syncDir(path.Dir(p))
}
func (t *Tree) restore(backup, p string) error {
	info, err := t.Root.Lstat(backup)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return t.renameExclusive(backup, p)
	}
	return t.Root.Link(backup, p)
}
func (t *Tree) install(p string, s Snapshot) error {
	switch s.Entry.Kind {
	case "":
		return nil
	case "dir":
		return t.Root.Mkdir(p, 0755)
	case "link":
		return t.Root.Symlink(s.Entry.Target, p)
	case "file":
		f, err := t.Root.OpenFile(s.Blob, os.O_RDWR|syscall.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		info, statErr := f.Stat()
		err = statErr
		if err == nil && info.Mode().Perm() != os.FileMode(s.Entry.Mode) {
			err = f.Chmod(os.FileMode(s.Entry.Mode))
			s.synced = false
		}
		if err == nil && !s.synced {
			err = f.Sync()
		}
		f.Close()
		if err != nil {
			return err
		}
		return t.renameExclusive(s.Blob, p)
	}
	return errors.New("unsupported entry")
}
func (t *Tree) syncDir(p string) error {
	f, err := t.Root.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func (t *Tree) WriteJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := Internal + "/spool/" + ID()
	f, err := t.Root.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer t.Root.Remove(tmp)
	err = writeAll(f, b)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = t.Root.Rename(tmp, p); err != nil {
		return err
	}
	return t.syncDir(path.Dir(p))
}
func Ordered(m Manifest, previous Manifest) []string {
	paths := make([]string, 0, len(m))
	for p := range m {
		paths = append(paths, p)
	}
	sort.Slice(paths, func(a, b int) bool {
		p, q := paths[a], paths[b]
		pd, qd := m[p].Kind == "", m[q].Kind == ""
		if pd != qd {
			return pd
		}
		if pd {
			if strings.Count(p, "/") != strings.Count(q, "/") {
				return strings.Count(p, "/") > strings.Count(q, "/")
			}
		}
		return p < q
	})
	return paths
}
