package mirror

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type Conflict struct {
	Path    string    `json:"path"`
	Base    Entry     `json:"base"`
	Local   Snapshot  `json:"local"`
	Remote  Snapshot  `json:"remote"`
	Reason  string    `json:"reason"`
	Updated time.Time `json:"updated"`
}
type State struct {
	Base      Manifest            `json:"base"`
	Conflicts map[string]Conflict `json:"conflicts"`
}

func LoadState(t *Tree) (*State, error) {
	s := &State{Base: Manifest{}, Conflicts: map[string]Conflict{}}
	b, err := t.Root.ReadFile(Internal + "/state.json")
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("refusing corrupt baseline: %w", err)
	}
	if s.Base == nil {
		s.Base = Manifest{}
	}
	if s.Conflicts == nil {
		s.Conflicts = map[string]Conflict{}
	}
	for p, e := range s.Base {
		if err := ValidateEntry(p, e); err != nil {
			return nil, fmt.Errorf("invalid persisted baseline: %w", err)
		}
	}
	for p, c := range s.Conflicts {
		if c.Path != p {
			return nil, errors.New("invalid persisted conflict path")
		}
		for _, snap := range []Snapshot{c.Local, c.Remote} {
			if err := ValidateEntry(p, snap.Entry); err != nil {
				return nil, err
			}
			if snap.Entry.Kind == "file" {
				id := strings.TrimPrefix(snap.Blob, Internal+"/spool/")
				b, e := hex.DecodeString(id)
				if e != nil || len(b) != 16 || snap.Blob != Internal+"/spool/"+id {
					return nil, errors.New("invalid persisted snapshot path")
				}
			} else if snap.Blob != "" {
				return nil, errors.New("unexpected persisted snapshot")
			}
		}
	}
	return s, nil
}
func (s *State) Save(t *Tree) error {
	for p, c := range s.Conflicts {
		if err := t.SyncSnapshot(&c.Local); err != nil {
			return err
		}
		if err := t.SyncSnapshot(&c.Remote); err != nil {
			return err
		}
		s.Conflicts[p] = c
	}
	if len(s.Conflicts) > 0 {
		if err := t.syncDir(Internal + "/spool"); err != nil {
			return err
		}
	}
	return t.WriteJSON(Internal+"/state.json", s)
}
func (s *State) SetBase(p string, e Entry) {
	if e.Kind == "" {
		delete(s.Base, p)
	} else {
		s.Base[p] = e
	}
	delete(s.Conflicts, p)
}
func (s *State) Conflict(t *Tree, p string, remote Snapshot, reason string) error {
	local, err := t.Snapshot(p, true)
	if err != nil {
		return err
	}
	// Never delete older conflict snapshots: they remain available for recovery.
	s.Conflicts[p] = Conflict{p, s.Base[p], local, remote, reason, time.Now()}
	return s.Save(t)
}
func Lock(t *Tree) (*os.File, error) {
	f, err := t.Root.OpenFile(Internal+"/lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another RCM worker owns this root")
	}
	return f, nil
}

type Resolution struct {
	Path   string `json:"path"`
	Choice string `json:"choice"`
	Local  Entry  `json:"local"`
	Remote Entry  `json:"remote"`
}

func QueueResolution(t *Tree, r Resolution) error {
	if err := ValidatePath(r.Path); err != nil {
		return err
	}
	if r.Choice != "local" && r.Choice != "remote" {
		return errors.New("choice must be local or remote")
	}
	if err := t.Root.MkdirAll(Internal+"/requests", 0700); err != nil {
		return err
	}
	return t.WriteJSON(Internal+"/requests/"+ID()+".json", r)
}
