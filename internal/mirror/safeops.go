package mirror

import (
	"golang.org/x/sys/unix"
	"path"
)

// Both descriptors originate from os.Root, so kernel *at operations cannot
// resolve an attacker-controlled absolute pathname or escape through a symlink.
func (t *Tree) renameExclusive(from, to string) error {
	a, err := t.Root.Open(path.Dir(from))
	if err != nil {
		return err
	}
	defer a.Close()
	b, err := t.Root.Open(path.Dir(to))
	if err != nil {
		return err
	}
	defer b.Close()
	return renameExclusive(int(a.Fd()), path.Base(from), int(b.Fd()), path.Base(to))
}
func (t *Tree) removeEmptyDirectory(p string) error {
	f, err := t.Root.Open(path.Dir(p))
	if err != nil {
		return err
	}
	defer f.Close()
	// Unlike os.Remove this cannot unlink a regular file swapped in by a writer.
	return unix.Unlinkat(int(f.Fd()), path.Base(p), unix.AT_REMOVEDIR)
}
