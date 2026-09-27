package cli

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"rcm/internal/mirror"
)

type Project struct {
	Name, Remote, Local, AgentDir string
	Config                        mirror.Config
	Verbose                       bool
	Created                       time.Time
}
type Runtime struct {
	State   string
	PID     int
	Updated time.Time
	Error   string
}
type Registry struct{ Dir string }

func stateDir() string {
	if s := os.Getenv("XDG_STATE_HOME"); s != "" {
		return filepath.Join(s, "rcm")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "rcm")
}
func registry() (Registry, error) {
	r := Registry{stateDir()}
	err := os.MkdirAll(r.Dir, 0700)
	return r, err
}

var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func (r Registry) dir(name string) string { return filepath.Join(r.Dir, name) }
func (r Registry) Load(name string) (Project, error) {
	var p Project
	if !nameRE.MatchString(name) {
		return p, errors.New("invalid project name")
	}
	b, err := os.ReadFile(filepath.Join(r.dir(name), "project.json"))
	if err != nil {
		return p, err
	}
	err = json.Unmarshal(b, &p)
	return p, err
}
func (r Registry) List() ([]Project, error) {
	ds, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil, err
	}
	var ps []Project
	for _, d := range ds {
		if !d.IsDir() {
			continue
		}
		p, err := r.Load(d.Name())
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return ps, nil
}
func atomicJSON(p string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".rcm-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(b)
	if err == nil {
		err = f.Sync()
	}
	ce := f.Close()
	if err == nil {
		err = ce
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), p)
}
func overlap(a, b string) bool {
	return a == b || strings.HasPrefix(a, b+string(os.PathSeparator)) || strings.HasPrefix(b, a+string(os.PathSeparator))
}
func (r Registry) Register(p Project) error {
	if !nameRE.MatchString(p.Name) {
		return errors.New("invalid --name; use letters, numbers, dots, underscores or hyphens")
	}
	lock, err := os.OpenFile(filepath.Join(r.Dir, "registry.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return err
	}
	ps, err := r.List()
	if err != nil {
		return err
	}
	for _, q := range ps {
		if q.Name == p.Name {
			if q.Remote == p.Remote && q.Local == p.Local {
				return nil
			}
			return fmt.Errorf("project name %s already used; choose --name", p.Name)
		}
		if overlap(q.Local, p.Local) {
			return fmt.Errorf("mirror overlaps project %s", q.Name)
		}
	}
	if err = os.MkdirAll(r.dir(p.Name), 0700); err != nil {
		return err
	}
	return atomicJSON(filepath.Join(r.dir(p.Name), "project.json"), p)
}
func (r Registry) runtime(name string) Runtime {
	var s Runtime
	b, err := os.ReadFile(filepath.Join(r.dir(name), "runtime.json"))
	if err == nil {
		_ = json.Unmarshal(b, &s)
	}
	return s
}
func (r Registry) saveRuntime(name string, s Runtime) error {
	s.Updated = time.Now()
	return atomicJSON(filepath.Join(r.dir(name), "runtime.json"), s)
}
func (r Registry) active(name string) bool {
	f, err := os.OpenFile(filepath.Join(r.dir(name), "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer f.Close()
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	return err != nil
}

func (r Registry) controlPath(name string) (string, error) {
	p := filepath.Join(r.dir(name), "control.sock")
	if len(p) < 100 {
		return p, nil
	}
	// Darwin's Unix socket path limit can be exceeded by an XDG/container path.
	dir := filepath.Join("/tmp", fmt.Sprintf("rcm-%d", os.Getuid()))
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, fs.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || int(st.Uid) != os.Getuid() {
		return "", fmt.Errorf("unsafe control socket directory %s", dir)
	}
	sum := sha256.Sum256([]byte(r.dir(name)))
	return filepath.Join(dir, fmt.Sprintf("%x.sock", sum[:16])), nil
}
