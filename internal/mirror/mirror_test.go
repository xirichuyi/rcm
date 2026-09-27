package mirror

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func tree(t *testing.T) *Tree {
	t.Helper()
	r, err := OpenTree(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
	return r
}
func put(t *testing.T, tr *Tree, p, s string) {
	t.Helper()
	full := filepath.Join(tr.Dir, p)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	tmp := full + ".writing"
	if err := os.WriteFile(tmp, []byte(s), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, full); err != nil {
		t.Fatal(err)
	}
}
func read(tr *Tree, p string) string { b, _ := tr.Root.ReadFile(p); return string(b) }
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	until := time.Now().Add(12 * time.Second)
	for time.Now().Before(until) {
		if fn() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatal("condition not reached before deadline")
}
func TestPathsAndSymlinks(t *testing.T) {
	tr := tree(t)
	for _, p := range []string{"", ".", "/etc/passwd", "../x", "a/../x", "a//b", "a/", Internal + "/x", "a/" + Internal + "/x", "a\x00b", "a\\b"} {
		if ValidatePath(p) == nil {
			t.Errorf("accepted %q", p)
		}
	}
	for _, target := range []string{"/etc/passwd", "../../etc/passwd", Internal + "/spool"} {
		e := Entry{Kind: "link", Target: target, Hash: Digest([]byte(target))}
		if ValidateEntry("link", e) == nil {
			t.Errorf("accepted target %q", target)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(tr.Dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Snapshot("escape/file", false); err == nil {
		t.Fatal("traversed symlink parent")
	}
	if err := tr.Apply("escape/file", Snapshot{Entry: Entry{Kind: "dir", Mode: 0755}}, Entry{}); err == nil {
		t.Fatal("wrote through symlink")
	}
	if err := os.Symlink("src", filepath.Join(tr.Dir, "relative")); err != nil {
		t.Fatal(err)
	}
	s, err := tr.Snapshot("relative", true)
	if err != nil || s.Entry.Target != "src" {
		t.Fatal(s, err)
	}
}
func TestProtocol(t *testing.T) {
	var buf bytes.Buffer
	w := NewWire(&buf, &buf)
	s := []byte("binary\x00body")
	m := Message{Type: "ENTRY", Path: "x", Entry: Entry{Kind: "file", Hash: Digest(s), Size: int64(len(s)), Mode: 0644}, Body: int64(len(s))}
	if err := w.Send(m, bytes.NewReader(s)); err != nil {
		t.Fatal(err)
	}
	got, err := w.Header()
	if err != nil {
		t.Fatal(err)
	}
	tr := tree(t)
	snap, err := tr.Receive(w, got)
	if err != nil {
		t.Fatal(err)
	}
	if read(tr, snap.Blob) != string(s) {
		t.Fatal("wrong payload")
	}
	for _, size := range []uint32{0, MaxHeader + 1} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], size)
		if _, err := NewWire(bytes.NewReader(b[:]), io.Discard).Header(); err == nil {
			t.Fatal("accepted invalid length")
		}
	}
	if _, err := NewWire(bytes.NewReader([]byte{0, 1}), io.Discard).Header(); err == nil {
		t.Fatal("accepted truncation")
	}
	if _, err := tr.Receive(NewWire(strings.NewReader("bad"), io.Discard), m); err == nil {
		t.Fatal("accepted truncated body")
	}
}
func TestIgnore(t *testing.T) {
	tr := tree(t)
	put(t, tr, ".gitignore", "*.tmp\n/out/\n*.txt\n")
	put(t, tr, "src/.gitignore", "!keep.txt\n")
	put(t, tr, ".rcmignore", "*.sqlite\n!readme.txt\n")
	i, err := LoadIgnore(tr, Config{Ignore: []string{"secret/"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{"node_modules/a.js": true, "x/a.log": true, ".git/config": true, "src/x.tmp": true, "out/x": true, "src/out/x": false, "a.sqlite": true, "src/keep.txt": false, "readme.txt": false, "a.txt": true, "secret/a": true, "src/main.go": false}
	for p, want := range cases {
		if got := i.Ignored(p, false); got != want {
			t.Errorf("%s=%v want %v", p, got, want)
		}
	}
	var glob Ignore
	glob.Add("", "a/**/b?.[ch]\n")
	for _, p := range []string{"a/b1.c", "a/x/y/b2.h"} {
		if !glob.Match(p, false) {
			t.Error("glob", p)
		}
	}
}
func TestApplyPreservesConflictAndHistory(t *testing.T) {
	tr := tree(t)
	put(t, tr, "x", "A")
	base, _ := tr.Snapshot("x", false)
	put(t, tr, "candidate", "C")
	candidate, _ := tr.Snapshot("candidate", true)
	put(t, tr, "x", "B")
	if err := tr.Apply("x", candidate, base.Entry); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if read(tr, "x") != "B" {
		t.Fatal("overwrote remote")
	}
	actual, _ := tr.Snapshot("x", false)
	if err := tr.Apply("x", candidate, actual.Entry); err != nil {
		t.Fatal(err)
	}
	if read(tr, "x") != "C" {
		t.Fatal("not applied")
	}
	ds, _ := os.ReadDir(filepath.Join(tr.Dir, Internal, "history"))
	found := false
	for _, d := range ds {
		b, _ := os.ReadFile(filepath.Join(tr.Dir, Internal, "history", d.Name()))
		if string(b) == "B" {
			found = true
		}
	}
	if !found {
		t.Fatal("previous version not retained")
	}
	put(t, tr, "dir/unknown", "keep")
	if err := tr.Apply("dir", Snapshot{}, Entry{Kind: "dir", Mode: 0755}); !errors.Is(err, ErrConflict) {
		t.Fatal("deleted nonempty dir", err)
	}
	if read(tr, "dir/unknown") != "keep" {
		t.Fatal("lost child")
	}
}

type session struct {
	stop func()
	done chan error
	logs *logCapture
}
type logCapture struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCapture) add(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}
func (l *logCapture) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, line := range l.lines {
		if strings.Contains(line, s) {
			n++
		}
	}
	return n
}
func startSession(t *testing.T, remote, local *Tree, c Config) *session {
	return startDelayedSession(t, remote, local, c, 0)
}
func startDelayedSession(t *testing.T, remote, local *Tree, c Config, delay time.Duration) *session {
	t.Helper()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 2)
	var aw, bw io.Writer = a, b
	if delay > 0 {
		aw = delayedWriter(ctx, a, delay)
		bw = delayedWriter(ctx, b, delay)
	}
	logs := &logCapture{}
	go func() { defer a.Close(); done <- RunAgent(ctx, remote, NewWire(a, aw)) }()
	go func() {
		defer b.Close()
		client := Client{Tree: local, Config: c, Verbose: true, Log: logs.add}
		done <- client.Session(ctx, NewWire(b, bw))
	}()
	var once sync.Once
	s := &session{done: done, logs: logs}
	s.stop = func() {
		once.Do(func() {
			cancel()
			a.Close()
			b.Close()
			for n := 0; n < 2; n++ {
				select {
				case err := <-done:
					if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "closed") {
						t.Log("session ended:", err)
					}
				case <-time.After(5 * time.Second):
					t.Error("session did not exit")
				}
			}
		})
	}
	t.Cleanup(s.stop)
	return s
}
func stable(t *testing.T, local *Tree, p, want string) {
	t.Helper()
	eventually(t, func() bool {
		if read(local, p) != want {
			return false
		}
		s, err := LoadState(local)
		return err == nil && s.Base[p].Hash == Digest([]byte(want))
	})
}
func TestIntegrationCRUDAndLoop(t *testing.T) {
	remote, local := tree(t), tree(t)
	put(t, remote, "src/a.go", "A")
	s := startSession(t, remote, local, Config{Debounce: "40ms"})
	stable(t, local, "src/a.go", "A")
	put(t, remote, "src/a.go", "B")
	stable(t, local, "src/a.go", "B")
	time.Sleep(200 * time.Millisecond)
	if s.logs.count("Local → Remote") != 0 {
		t.Fatal("feedback loop", s.logs.lines)
	}
	put(t, local, "src/a.go", "C")
	eventually(t, func() bool { return read(remote, "src/a.go") == "C" })
	stable(t, local, "src/a.go", "C")
	if err := os.Rename(filepath.Join(remote.Dir, "src/a.go"), filepath.Join(remote.Dir, "src/b.go")); err != nil {
		t.Fatal(err)
	}
	stable(t, local, "src/b.go", "C")
	eventually(t, func() bool { _, err := local.Root.Lstat("src/a.go"); return os.IsNotExist(err) })
	if err := remote.Root.Mkdir("empty", 0755); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { i, e := local.Root.Lstat("empty"); return e == nil && i.IsDir() })
	remote.Root.Remove("empty")
	eventually(t, func() bool { _, e := local.Root.Lstat("empty"); return os.IsNotExist(e) })
	remote.Root.Remove("src/b.go")
	remote.Root.Remove("src")
	eventually(t, func() bool { _, e := local.Root.Lstat("src"); return os.IsNotExist(e) })
}
func TestBurstCoalesce(t *testing.T) {
	remote, local := tree(t), tree(t)
	s := startSession(t, remote, local, Config{Debounce: "100ms"})
	eventually(t, func() bool { return s.logs.count("Initial sync complete") > 0 })
	for n := 0; n < 100; n++ {
		p := fmt.Sprintf("src/f%03d.go", n)
		for v := 0; v < 3; v++ {
			put(t, remote, p, fmt.Sprintf("final-%d-%d", n, v))
		}
	}
	eventually(t, func() bool {
		for n := 0; n < 100; n++ {
			if read(local, fmt.Sprintf("src/f%03d.go", n)) != fmt.Sprintf("final-%d-2", n) {
				return false
			}
		}
		return true
	})
	time.Sleep(200 * time.Millisecond)
	if n := s.logs.count("Remote → Local"); n > 130 {
		t.Fatalf("poor coalescing: %d events", n)
	}
}
func TestDisconnectReconcileAndConflict(t *testing.T) {
	remote, local := tree(t), tree(t)
	for n := 0; n < 30; n++ {
		put(t, remote, fmt.Sprintf("f%02d", n), "A")
	}
	s := startSession(t, remote, local, Config{Debounce: "40ms"})
	stable(t, local, "f29", "A")
	s.stop()
	for n := 0; n < 20; n++ {
		put(t, remote, fmt.Sprintf("f%02d", n), "B")
	}
	for n := 20; n < 25; n++ {
		remote.Root.Remove(fmt.Sprintf("f%02d", n))
	}
	for n := 30; n < 40; n++ {
		put(t, remote, fmt.Sprintf("f%02d", n), "new")
	}
	put(t, local, "f00", "C")
	s2 := startSession(t, remote, local, Config{Debounce: "40ms"})
	stable(t, local, "f39", "new")
	eventually(t, func() bool { st, err := LoadState(local); return err == nil && st.Conflicts["f00"].Path != "" })
	if read(remote, "f00") != "B" || read(local, "f00") != "C" {
		t.Fatal("conflict destroyed content")
	}
	for n := 1; n < 20; n++ {
		if read(local, fmt.Sprintf("f%02d", n)) != "B" {
			t.Fatal("missed modification", n)
		}
	}
	for n := 20; n < 25; n++ {
		if _, err := local.Root.Lstat(fmt.Sprintf("f%02d", n)); !os.IsNotExist(err) {
			t.Fatal("missed deletion", n)
		}
	}
	st, _ := LoadState(local)
	cf := st.Conflicts["f00"]
	if read(local, cf.Remote.Blob) != "B" || read(local, cf.Local.Blob) != "C" {
		t.Fatal("conflict snapshot missing")
	}
	if err := QueueResolution(local, Resolution{"f00", "local", cf.Local.Entry, cf.Remote.Entry}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool {
		st, e := LoadState(local)
		return e == nil && len(st.Conflicts) == 0 && read(remote, "f00") == "C"
	})
	s2.stop()
	// A fresh worker must use the durable baseline, not rediscover an old conflict.
	s3 := startSession(t, remote, local, Config{Debounce: "40ms"})
	eventually(t, func() bool { return s3.logs.count("Initial sync complete") > 0 })
	st, _ = LoadState(local)
	if len(st.Conflicts) != 0 {
		t.Fatal(st.Conflicts)
	}
}
func TestLargeStreaming(t *testing.T) {
	if testing.Short() {
		t.Skip("large streaming test")
	}
	for _, size := range []int64{10 << 20, 100 << 20} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			remote, local := tree(t), tree(t)
			f, err := remote.Root.OpenFile("large.bin", os.O_CREATE|os.O_WRONLY, 0644)
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Truncate(size); err != nil {
				t.Fatal(err)
			}
			f.Close()
			s := startSession(t, remote, local, Config{Debounce: "40ms"})
			eventually(t, func() bool {
				info, err := local.Root.Lstat("large.bin")
				if err != nil || info.Size() != size {
					return false
				}
				state, e := LoadState(local)
				return e == nil && state.Base["large.bin"].Size == size
			})
			s.stop()
			a, err := remote.Snapshot("large.bin", false)
			if err != nil {
				t.Fatal(err)
			}
			b, err := local.Snapshot("large.bin", false)
			if err != nil || !Equal(a.Entry, b.Entry) {
				t.Fatal("large file corrupt", err)
			}
		})
	}
}
func TestConflictDecisionTable(t *testing.T) {
	for _, tc := range []struct {
		name, base, local, remote, want string
		conflict                        bool
	}{{"remote only", "A", "A", "B", "B", false}, {"local only", "A", "C", "A", "C", false}, {"both", "A", "C", "B", "C", true}, {"equal", "A", "B", "B", "B", false}, {"delete modify", "A", "C", "", "C", true}, {"local delete remote modify", "A", "", "B", "", true}, {"new collision", "", "C", "B", "C", true}} {
		t.Run(tc.name, func(t *testing.T) {
			tr := tree(t)
			source := tree(t)
			state := &State{Base: Manifest{}, Conflicts: map[string]Conflict{}}
			if tc.base != "" {
				put(t, tr, "x", tc.base)
				b, _ := tr.Snapshot("x", false)
				state.Base["x"] = b.Entry
				tr.Root.Remove("x")
			}
			if tc.local != "" {
				put(t, tr, "x", tc.local)
			}
			if tc.remote != "" {
				put(t, source, "x", tc.remote)
			}
			rs, _ := source.Snapshot("x", true)
			var wire bytes.Buffer
			source.Send(NewWire(nil, &wire), "ENTRY", "x", rs, Entry{})
			w := NewWire(&wire, nil)
			m, _ := w.Header()
			snap, err := tr.Receive(w, m)
			if err != nil {
				t.Fatal(err)
			}
			c := Client{Tree: tr, State: state, remote: Manifest{}, pending: map[string]Entry{}}
			if err = c.process("x", snap); err != nil {
				t.Fatal(err)
			}
			if read(tr, "x") != tc.want || (len(state.Conflicts) > 0) != tc.conflict {
				b, _ := json.Marshal(state)
				t.Fatalf("value %q state %s", read(tr, "x"), b)
			}
		})
	}
}

func TestCrashRecoveryAndNoClobber(t *testing.T) {
	tr := tree(t)
	put(t, tr, "x", "A")
	s, _ := tr.Snapshot("x", false)
	backup := Internal + "/history/" + ID()
	r := recovery{"x", s.Entry, Entry{}, backup, time.Now()}
	if err := tr.WriteJSON(backup+".json", r); err != nil {
		t.Fatal(err)
	}
	tr.Root.Rename("x", backup)
	if err := tr.Recover(); err != nil {
		t.Fatal(err)
	}
	if read(tr, "x") != "A" {
		t.Fatal("interrupted move not restored")
	}
	// A concurrent replacement wins the destination; the moved version stays recoverable.
	put(t, tr, "x", "B")
	backup2 := Internal + "/history/" + ID()
	tr.Root.Rename("x", backup2)
	put(t, tr, "x", "C")
	if err := tr.restore(backup2, "x"); err == nil {
		t.Fatal("restore clobbered competing file")
	}
	if read(tr, "x") != "C" || read(tr, backup2) != "B" {
		t.Fatal("lost competing versions")
	}
	// An intentional completed deletion must never be resurrected at restart.
	current, _ := tr.Snapshot("x", false)
	if err := tr.Apply("x", Snapshot{}, current.Entry); err != nil {
		t.Fatal(err)
	}
	if err := tr.Recover(); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Root.Lstat("x"); !os.IsNotExist(err) {
		t.Fatal("completed delete resurrected")
	}
	put(t, tr, "not-a-dir", "keep")
	if err := tr.removeEmptyDirectory("not-a-dir"); err == nil {
		t.Fatal("rmdir unlinked a file")
	}
	if read(tr, "not-a-dir") != "keep" {
		t.Fatal("file lost")
	}
}
func TestReadOnlyAndGitNeverUpload(t *testing.T) {
	remote, local := tree(t), tree(t)
	put(t, remote, "file", "A")
	put(t, remote, ".git/config", "remote-config")
	disabled := false
	c := Config{Debounce: "40ms", IncludeGit: true}
	c.LocalWrite.Enabled = &disabled
	s := startSession(t, remote, local, c)
	stable(t, local, "file", "A")
	stable(t, local, ".git/config", "remote-config")
	put(t, local, "file", "local")
	put(t, local, ".git/config", "local-config")
	time.Sleep(200 * time.Millisecond)
	if read(remote, "file") != "A" || read(remote, ".git/config") != "remote-config" {
		t.Fatal("read-only upload")
	}
	s.stop()
	enabled := true
	c.LocalWrite.Enabled = &enabled
	s2 := startSession(t, remote, local, c)
	eventually(t, func() bool { return read(remote, "file") == "local" })
	time.Sleep(200 * time.Millisecond)
	if read(remote, ".git/config") != "remote-config" {
		t.Fatal("Git metadata uploaded")
	}
	s2.stop()
}
func TestRemoteResolutionAndStaleChoice(t *testing.T) {
	remote, local := tree(t), tree(t)
	put(t, remote, "x", "A")
	s := startSession(t, remote, local, Config{Debounce: "40ms"})
	stable(t, local, "x", "A")
	s.stop()
	put(t, remote, "x", "B")
	put(t, local, "x", "C")
	s2 := startSession(t, remote, local, Config{Debounce: "40ms"})
	eventually(t, func() bool { st, e := LoadState(local); return e == nil && st.Conflicts["x"].Path != "" })
	st, _ := LoadState(local)
	cf := st.Conflicts["x"]
	put(t, local, "x", "D")
	QueueResolution(local, Resolution{"x", "remote", cf.Local.Entry, cf.Remote.Entry})
	eventually(t, func() bool { return s2.logs.count("Resolution rejected") > 0 })
	if read(local, "x") != "D" {
		t.Fatal("stale resolution overwrote edit")
	}
	now, _ := local.Snapshot("x", false)
	QueueResolution(local, Resolution{"x", "remote", now.Entry, cf.Remote.Entry})
	eventually(t, func() bool {
		st, e := LoadState(local)
		return e == nil && len(st.Conflicts) == 0 && read(local, "x") == "B"
	})
	put(t, local, "x", "E")
	eventually(t, func() bool { return read(remote, "x") == "E" })
	if read(local, cf.Remote.Blob) != "B" {
		t.Fatal("resolution mutated archived conflict blob")
	}
}

type delayedChunk struct {
	bytes []byte
	at    time.Time
}
type flightWriter struct {
	ctx   context.Context
	queue chan delayedChunk
	delay time.Duration
}

func delayedWriter(ctx context.Context, w io.Writer, delay time.Duration) io.Writer {
	f := &flightWriter{ctx: ctx, queue: make(chan delayedChunk, 128), delay: delay}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case item := <-f.queue:
				timer := time.NewTimer(time.Until(item.at))
				select {
				case <-ctx.Done():
					timer.Stop()
					return
				case <-timer.C:
				}
				if writeAll(w, item.bytes) != nil {
					return
				}
			}
		}
	}()
	return f
}
func (f *flightWriter) Write(b []byte) (int, error) {
	item := delayedChunk{append([]byte(nil), b...), time.Now().Add(f.delay)}
	select {
	case <-f.ctx.Done():
		return 0, f.ctx.Err()
	case f.queue <- item:
		return len(b), nil
	}
}
func TestSimulatedRTT300ms(t *testing.T) {
	remote, local := tree(t), tree(t)
	put(t, remote, "x", "A")
	s := startDelayedSession(t, remote, local, Config{Debounce: "150ms"}, 150*time.Millisecond)
	stable(t, local, "x", "A")
	for _, size := range []int{10 << 10, 500 << 10} {
		content := strings.Repeat("B", size)
		began := time.Now()
		put(t, remote, "x", content)
		eventually(t, func() bool { return read(local, "x") == content })
		elapsed := time.Since(began)
		t.Logf("%d KiB, simulated 300ms RTT: %s", size>>10, elapsed)
		if elapsed > time.Second {
			t.Fatalf("missed latency target: %s", elapsed)
		}
		stable(t, local, "x", content)
	}
	s.stop()
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }
func TestLargeReceiveBoundedAllocation(t *testing.T) {
	if testing.Short() {
		t.Skip("100MiB streaming allocation measurement")
	}
	tr := tree(t)
	const size = int64(100 << 20)
	h := sha256.New()
	io.CopyN(h, zeroReader{}, size)
	entry := Entry{Kind: "file", Size: size, Hash: hex.EncodeToString(h.Sum(nil)), Mode: 0644}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s, err := tr.Receive(NewWire(io.LimitReader(zeroReader{}, size), nil), Message{Type: "ENTRY", Path: "large", Entry: entry, Body: size})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	tr.Discard(s)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("100MiB body: allocated %.2f MiB", float64(allocated)/(1<<20))
	if allocated > 8<<20 {
		t.Fatalf("body buffered in memory: %d bytes allocated", allocated)
	}
}

func TestScale20000(t *testing.T) {
	if os.Getenv("RCM_SCALE_TEST") != "1" {
		t.Skip("set RCM_SCALE_TEST=1 for 20,000-file integration")
	}
	remote, local := tree(t), tree(t)
	for n := 0; n < 20000; n++ {
		put(t, remote, fmt.Sprintf("d%03d/f%05d.go", n/200, n), "package example\n")
	}
	began := time.Now()
	s := startSession(t, remote, local, Config{Debounce: "150ms"})
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) && s.logs.count("Initial sync complete") == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if s.logs.count("Initial sync complete") == 0 {
		t.Fatal("20,000-file sync timed out")
	}
	t.Logf("20,000 files + 100 directories: initial sync %s", time.Since(began))
	state, err := LoadState(local)
	if err != nil || len(state.Base) != 20100 {
		t.Fatalf("baseline entries: %d: %v", len(state.Base), err)
	}
	began = time.Now()
	put(t, remote, "d099/f19999.go", "updated")
	eventually(t, func() bool { return read(local, "d099/f19999.go") == "updated" })
	t.Logf("first update during initial catch-up in 20k tree: %s", time.Since(began))
	stable(t, local, "d099/f19999.go", "updated")
	began = time.Now()
	put(t, remote, "d050/f10000.go", "steady update")
	eventually(t, func() bool { return read(local, "d050/f10000.go") == "steady update" })
	t.Logf("steady-state update in 20k tree: %s", time.Since(began))
	s.stop()
}

type cutoffWriter struct {
	conn      net.Conn
	remaining int
}

func (w *cutoffWriter) Write(b []byte) (int, error) {
	if w.remaining <= 0 {
		w.conn.Close()
		return 0, io.ErrUnexpectedEOF
	}
	if len(b) > w.remaining {
		n, err := w.conn.Write(b[:w.remaining])
		w.remaining -= n
		w.conn.Close()
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	n, err := w.conn.Write(b)
	w.remaining -= n
	return n, err
}
func TestDisconnectDuringBody(t *testing.T) {
	remote, local := tree(t), tree(t)
	f, err := remote.Root.OpenFile("large", os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	f.Truncate(10 << 20)
	f.Close()
	a, b := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 2)
	go func() { defer a.Close(); done <- RunAgent(ctx, remote, NewWire(a, &cutoffWriter{a, 1 << 20})) }()
	go func() { defer b.Close(); client := Client{Tree: local}; done <- client.Session(ctx, NewWire(b, b)) }()
	for n := 0; n < 2; n++ {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("truncated transfer succeeded")
			}
		case <-time.After(10 * time.Second):
			a.Close()
			b.Close()
			t.Fatal("truncated transfer hung")
		}
	}
	if _, err := local.Root.Lstat("large"); !os.IsNotExist(err) {
		t.Fatal("partial file became visible")
	}
	state, err := LoadState(local)
	if err != nil {
		t.Fatal(err)
	}
	if state.Base["large"].Kind != "" {
		t.Fatal("partial transfer advanced baseline")
	}
	s := startSession(t, remote, local, Config{Debounce: "40ms"})
	eventually(t, func() bool { st, e := LoadState(local); return e == nil && st.Base["large"].Size == 10<<20 })
	s.stop()
}
func TestOnlineSimultaneousConflict(t *testing.T) {
	remote, local := tree(t), tree(t)
	put(t, remote, "x", "A")
	s := startSession(t, remote, local, Config{Debounce: "150ms"})
	stable(t, local, "x", "A")
	put(t, remote, "x", "B")
	put(t, local, "x", "C")
	eventually(t, func() bool { st, e := LoadState(local); return e == nil && st.Conflicts["x"].Path != "" })
	if read(remote, "x") != "B" || read(local, "x") != "C" {
		t.Fatal("silently overwrote simultaneous edits")
	}
	s.stop()
}

func TestCorruptPersistedPathsRejected(t *testing.T) {
	tr := tree(t)
	s := State{Base: Manifest{"../outside": Entry{Kind: "dir", Mode: 0755}}, Conflicts: map[string]Conflict{}}
	tr.WriteJSON(Internal+"/state.json", s)
	if _, err := LoadState(tr); err == nil {
		t.Fatal("accepted escaping baseline")
	}
	s.Base = Manifest{}
	s.Conflicts["x"] = Conflict{Path: "x", Remote: Snapshot{Entry: Entry{Kind: "file", Hash: Digest(nil), Mode: 0644}, Blob: "../../outside"}}
	tr.WriteJSON(Internal+"/state.json", s)
	if _, err := LoadState(tr); err == nil {
		t.Fatal("accepted escaping blob path")
	}
}
