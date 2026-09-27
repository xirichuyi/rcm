package mirror

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const Protocol = 1
const Internal = ".rcm-internal"

var ErrConflict = errors.New("concurrent change: conflict")
var ErrUnstable = errors.New("file changed while reading")

type Entry struct {
	Kind   string `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	Mtime  int64  `json:"mtime,omitempty"`
	Hash   string `json:"hash,omitempty"`
	Mode   uint32 `json:"mode,omitempty"`
	Target string `json:"target,omitempty"`
}
type Manifest map[string]Entry

func Equal(a, b Entry) bool {
	return a.Kind == b.Kind && a.Hash == b.Hash && a.Mode == b.Mode && a.Target == b.Target
}
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ValidatePath(p string) error {
	if p == "" || p == "." || path.IsAbs(p) || path.Clean(p) != p || strings.ContainsAny(p, "\\\x00\r\n") {
		return fmt.Errorf("invalid path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if s == ".." || s == Internal {
			return fmt.Errorf("reserved/escaping path %q", p)
		}
	}
	return nil
}
func ValidateEntry(p string, e Entry) error {
	if err := ValidatePath(p); err != nil {
		return err
	}
	switch e.Kind {
	case "":
		if e.Size != 0 || e.Hash != "" || e.Target != "" || e.Mode != 0 {
			return errors.New("invalid tombstone")
		}
	case "dir":
		if e.Size != 0 || e.Hash != "" || e.Target != "" || e.Mode != 0755 {
			return errors.New("directory body")
		}
	case "file":
		h, err := hex.DecodeString(e.Hash)
		if err != nil || len(h) != 32 || e.Size < 0 || e.Size > MaxBody || e.Target != "" || (e.Mode != 0644 && e.Mode != 0755) {
			return errors.New("invalid file hash/size")
		}
	case "link":
		if e.Size != 0 || e.Mode != 0 {
			return errors.New("invalid symlink metadata")
		}
		dest := path.Clean(path.Join(path.Dir(p), e.Target))
		if e.Target == "" || path.IsAbs(e.Target) || strings.ContainsAny(e.Target, "\\\x00") || dest == ".." || strings.HasPrefix(dest, "../") || dest == Internal || strings.HasPrefix(dest, Internal+"/") {
			return fmt.Errorf("unsafe symlink %q", e.Target)
		}
		if Digest([]byte(e.Target)) != e.Hash {
			return errors.New("invalid link hash")
		}
	default:
		return fmt.Errorf("unsupported entry kind %q", e.Kind)
	}
	if e.Mode != 0 && e.Mode != 0644 && e.Mode != 0755 {
		return errors.New("invalid mode")
	}
	return nil
}
func GitPath(p string) bool {
	for _, s := range strings.Split(p, "/") {
		if s == ".git" {
			return true
		}
	}
	return false
}

type Config struct {
	Ignore     []string `yaml:"ignore" json:"ignore,omitempty"`
	Debounce   string   `yaml:"debounce" json:"debounce,omitempty"`
	LocalWrite struct {
		Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	} `yaml:"local_write" json:"local_write"`
	Conflict struct {
		Strategy string `yaml:"strategy" json:"strategy,omitempty"`
	} `yaml:"conflict" json:"conflict"`
	IncludeGit bool `yaml:"-" json:"include_git"`
}

func (c Config) Duration() time.Duration {
	d, _ := time.ParseDuration(c.Debounce)
	if d == 0 {
		return 150 * time.Millisecond
	}
	return d
}
func (c Config) Writable() bool { return c.LocalWrite.Enabled == nil || *c.LocalWrite.Enabled }
func (c Config) Validate() error {
	if c.Debounce != "" {
		d, err := time.ParseDuration(c.Debounce)
		if err != nil || d < 10*time.Millisecond || d > 5*time.Second {
			return errors.New("debounce must be between 10ms and 5s")
		}
	}
	if c.Conflict.Strategy != "" && c.Conflict.Strategy != "manual" {
		return errors.New("only conflict.strategy: manual is supported")
	}
	return nil
}
func MergeConfig(base, over Config) Config {
	base.Ignore = append(append([]string{}, base.Ignore...), over.Ignore...)
	if over.Debounce != "" {
		base.Debounce = over.Debounce
	}
	if over.LocalWrite.Enabled != nil {
		base.LocalWrite.Enabled = over.LocalWrite.Enabled
	}
	if over.Conflict.Strategy != "" {
		base.Conflict.Strategy = over.Conflict.Strategy
	}
	base.IncludeGit = over.IncludeGit
	return base
}
