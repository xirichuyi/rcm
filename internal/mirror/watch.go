package mirror

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

type Watch struct {
	Changes chan []string
	Errors  chan error
	done    chan struct{}
	w       *fsnotify.Watcher
}

func NewWatch(t *Tree, i *Ignore, delay time.Duration) (*Watch, error) {
	prepareWatchLimits()
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	w := &Watch{Changes: make(chan []string, 1), Errors: make(chan error, 1), done: make(chan struct{}), w: fw}
	var add func(string) error
	add = func(p string) error {
		full := filepath.Join(t.Dir, filepath.FromSlash(p))
		if err := fw.Add(full); err != nil {
			return fmt.Errorf("watch %s (check inotify/kqueue limits): %w", p, err)
		}
		f, err := t.Root.Open(p)
		if err != nil {
			return err
		}
		ds, err := f.ReadDir(-1)
		f.Close()
		if err != nil {
			return err
		}
		for _, d := range ds {
			q := path.Join(p, d.Name())
			if d.IsDir() && !i.Match(q, true) {
				if err := add(q); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
			}
		}
		return nil
	}
	// Drain before recursive registration: startup changes cannot fill the OS queue.

	go func() {
		defer close(w.done)
		ticker := time.NewTicker(delay)
		defer ticker.Stop()
		dirty := map[string]bool{}
		for {
			select {
			case e, ok := <-fw.Events:
				if !ok {
					return
				}
				rel, err := filepath.Rel(t.Dir, e.Name)
				if err != nil {
					continue
				}
				p := filepath.ToSlash(rel)
				if ValidatePath(p) != nil {
					continue
				}
				if i.Ignored(p, false) {
					continue
				}
				dirty[p] = true
				if e.Has(fsnotify.Create) {
					info, err := t.Root.Lstat(p)
					if err == nil && info.IsDir() && !i.Ignored(p, true) {
						if err = add(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
							select {
							case w.Errors <- err:
							default:
							}
						}
					}
				}
			case err, ok := <-fw.Errors:
				if !ok {
					return
				}
				select {
				case w.Errors <- fmt.Errorf("watch lost events; reconciliation required: %w", err):
				default:
				}
			case <-ticker.C:
				if len(dirty) > 0 {
					ps := make([]string, 0, len(dirty))
					for p := range dirty {
						ps = append(ps, p)
					}
					select {
					case w.Changes <- ps:
						dirty = map[string]bool{}
					default:
					}
				}
			}
		}
	}()
	if err := add("."); err != nil {
		fw.Close()
		<-w.done
		return nil, err
	}
	return w, nil
}
func (w *Watch) Close() { w.w.Close(); <-w.done }
func (i *Ignore) Ignored(p string, dir bool) bool {
	parts := strings.Split(p, "/")
	for n := 1; n < len(parts); n++ {
		if i.Match(strings.Join(parts[:n], "/"), true) {
			return true
		}
	}
	return i.Match(p, dir)
}
func (t *Tree) Update(i *Ignore, old Manifest, paths []string) (Manifest, error) {
	next := Manifest{}
	for p, e := range old {
		next[p] = e
	}
	paths = compactPaths(paths)
	for _, p := range paths {
		if p == "." {
			m, _, err := t.Scan(i)
			return m, err
		}
		if old[p].Kind == "dir" {
			for q := range next {
				if q == p || strings.HasPrefix(q, p+"/") {
					delete(next, q)
				}
			}
		} else {
			delete(next, p)
		}
		var walk func(string) error
		walk = func(q string) error {
			info, err := t.Root.Lstat(q)
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if i.Ignored(q, info.IsDir()) {
				return nil
			}
			s, err := t.Snapshot(q, false)
			if err != nil {
				return err
			}
			if s.Entry.Kind == "" {
				return nil
			}
			next[q] = s.Entry
			if info.IsDir() {
				f, err := t.Root.Open(q)
				if err != nil {
					return err
				}
				ds, err := f.ReadDir(-1)
				f.Close()
				if err != nil {
					return err
				}
				for _, d := range ds {
					if err := walk(path.Join(q, d.Name())); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := walk(p); err != nil {
			return nil, err
		}
	}
	return next, nil
}
func Changes(old, next Manifest) Manifest {
	m := Manifest{}
	for p, e := range next {
		if !Equal(old[p], e) {
			m[p] = e
		}
	}
	for p := range old {
		if _, ok := next[p]; !ok {
			m[p] = Entry{}
		}
	}
	return m
}
func RuleChanged(paths []string) bool {
	for _, p := range paths {
		b := path.Base(p)
		if b == ".gitignore" || p == ".rcmignore" || p == ".rcm.yaml" {
			return true
		}
	}
	return false
}

func compactPaths(paths []string) []string {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	out := paths[:0]
	for _, p := range paths {
		if p == "." {
			return []string{"."}
		}
		if len(out) > 0 && (p == out[len(out)-1] || strings.HasPrefix(p, out[len(out)-1]+"/")) {
			continue
		}
		out = append(out, p)
	}
	return out
}
