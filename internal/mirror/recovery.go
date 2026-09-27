package mirror

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"syscall"
	"time"
)

type recovery struct {
	Path          string
	Before, After Entry
	Backup        string
	Time          time.Time
}

func (t *Tree) finishRecovery(backup string) error {
	return t.WriteJSON(backup+".done", struct{ Done bool }{true})
}

// Recover is called while holding the root lock, before scanning or watching.
// It restores an interrupted move only if the destination is still absent.
// Competing destination contents are always kept; history is never deleted.
func (t *Tree) Recover() error {
	f, err := t.Root.Open(Internal + "/history")
	if err != nil {
		return err
	}
	ds, err := f.ReadDir(-1)
	f.Close()
	if err != nil {
		return err
	}
	for _, d := range ds {
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), ".json") {
			continue
		}
		name := path.Join(Internal, "history", d.Name())
		backup := strings.TrimSuffix(name, ".json")
		if _, err = t.Root.Lstat(backup + ".done"); err == nil {
			continue
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		b, err := t.Root.ReadFile(name)
		if err != nil {
			return err
		}
		var r recovery
		if err = json.Unmarshal(b, &r); err != nil {
			return fmt.Errorf("corrupt recovery journal: %w", err)
		}
		if r.Backup != backup {
			return errors.New("invalid recovery backup path")
		}
		if err = ValidatePath(r.Path); err != nil {
			return err
		}
		if _, err = t.Root.Lstat(backup); errors.Is(err, fs.ErrNotExist) {
			continue
		} else if err != nil {
			return err
		}
		if _, err = t.Root.Lstat(r.Path); errors.Is(err, fs.ErrNotExist) {
			if err = t.Parents(r.Path); err != nil {
				return fmt.Errorf("recovery needs manual attention (%s): %w", backup, err)
			}
			if err = t.restore(backup, r.Path); err != nil && !errors.Is(err, fs.ErrExist) {
				return err
			}
			if err = t.syncDir(path.Dir(r.Path)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if err = t.finishRecovery(backup); err != nil {
			return err
		}
	}
	return nil
}
func (t *Tree) Clone(s Snapshot) (Snapshot, error) {
	if s.Entry.Kind != "file" {
		return s, nil
	}
	in, err := t.Root.Open(s.Blob)
	if err != nil {
		return Snapshot{}, err
	}
	defer in.Close()
	out := Snapshot{Entry: s.Entry, Blob: Internal + "/spool/" + ID()}
	f, err := t.Root.OpenFile(out.Blob, os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(s.Entry.Mode))
	if err != nil {
		return Snapshot{}, err
	}
	_, err = io.Copy(f, in)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		t.Discard(out)
	}
	out.synced = err == nil
	return out, err
}

func (t *Tree) SyncSnapshot(s *Snapshot) error {
	if s.Entry.Kind != "file" || s.synced {
		return nil
	}
	f, err := t.Root.OpenFile(s.Blob, os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Sync(); err != nil {
		return err
	}
	s.synced = true
	return nil
}
