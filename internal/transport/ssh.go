package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"

	"rcm/internal/assets"
)

var hostPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@-]*$`)

type Target struct{ Host, Root string }

func Parse(s string) (Target, error) {
	host, root, ok := strings.Cut(s, ":")
	if !ok || !hostPattern.MatchString(host) || !path.IsAbs(root) || strings.ContainsAny(root, "\x00\r\n") {
		return Target{}, fmt.Errorf("expected SSH-alias:/absolute/project/path (IPv6: use an SSH alias)")
	}
	root = path.Clean(root)
	if root == "/" {
		return Target{}, fmt.Errorf("refusing to mirror filesystem root")
	}
	return Target{host, root}, nil
}
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type SSH struct {
	Target     Target
	AgentDir   string
	Stderr     io.Writer
	Executable string
}

func (s SSH) command(ctx context.Context, script string) *exec.Cmd {
	bin := s.Executable
	if bin == "" {
		bin = "ssh"
	}
	cmd := exec.CommandContext(ctx, bin, "-T", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=15", "--", s.Target.Host, script)
	cmd.Stderr = s.Stderr
	return cmd
}
func (s SSH) run(ctx context.Context, script string, input io.Reader) ([]byte, error) {
	cmd := s.command(ctx, script)
	cmd.Stdin = input
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("SSH setup: %w", err)
	}
	return out, nil
}
func (s SSH) Agent(ctx context.Context) (string, error) {
	out, err := s.run(ctx, "uname -s; uname -m", nil)
	if err != nil {
		return "", err
	}
	fields := strings.Fields(string(out))
	if len(fields) != 2 {
		return "", fmt.Errorf("unexpected uname output: %q", out)
	}
	goos := strings.ToLower(fields[0])
	arch := fields[1]
	switch arch {
	case "x86_64", "amd64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	default:
		return "", fmt.Errorf("unsupported remote architecture %s", arch)
	}
	if goos != "linux" && goos != "darwin" {
		return "", fmt.Errorf("unsupported remote OS %s", goos)
	}
	var data []byte
	if s.AgentDir != "" {
		data, err = os.ReadFile(filepath.Join(s.AgentDir, "rcm-"+goos+"-"+arch))
	} else {
		data, err = assets.Files.ReadFile("agents/rcm-" + goos + "-" + arch)
		if err != nil && runtime.GOOS == goos && runtime.GOARCH == arch {
			exe, e := os.Executable()
			if e == nil {
				data, err = os.ReadFile(exe)
			}
		}
	}
	if err != nil {
		return "", fmt.Errorf("agent %s-%s unavailable; run scripts/release.sh or pass --agent-dir: %w", goos, arch, err)
	}
	h := sha256.Sum256(data)
	sum := hex.EncodeToString(h[:])
	name := "rcm-" + sum
	// No remote toolchain or checksum utility required. Cache is content addressed;
	// protocol HELLO checks compatibility after executing it.
	probe := "test -x \"$HOME/.cache/rcm/" + name + "\" && printf ready || printf missing"
	out, err = s.run(ctx, probe, nil)
	if err != nil {
		return "", err
	}
	if string(out) != "ready" {
		script := "set -eu; umask 077; d=\"$HOME/.cache/rcm\"; mkdir -p \"$d\"; t=\"$d/.upload-" + sum + "-$$\"; trap 'rm -f \"$t\"' EXIT HUP INT TERM; cat > \"$t\"; chmod 700 \"$t\"; mv -f \"$t\" \"$d/" + name + "\""
		if _, err = s.run(ctx, script, bytes.NewReader(data)); err != nil {
			return "", err
		}
	}
	return "\"$HOME/.cache/rcm/" + name + "\"", nil
}

type Stream struct {
	Reader io.ReadCloser
	Writer io.WriteCloser
	cmd    *exec.Cmd
	once   sync.Once
	err    error
}

func (s SSH) Open(ctx context.Context, agent string) (*Stream, error) {
	cmd := s.command(ctx, "exec "+agent+" agent --root "+Quote(s.Target.Root))
	r, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	w, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, err
	}
	return &Stream{Reader: r, Writer: w, cmd: cmd}, nil
}
func (s *Stream) Close() error {
	s.once.Do(func() {
		s.Writer.Close()
		s.Reader.Close()
		if s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
		s.err = s.cmd.Wait()
	})
	return s.err
}
