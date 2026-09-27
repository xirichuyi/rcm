package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"rcm/internal/transport"
)

func TestCLIWithSSHShim(t *testing.T) {
	if testing.Short() {
		t.Skip("CLI subprocess integration")
	}
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "rcm")
	cmd := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-buildvcs=false", "-o", bin, "../../cmd/rcm")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	localHome := filepath.Join(tmp, "local-home")
	remoteHome := filepath.Join(tmp, "remote-home")
	remote := filepath.Join(tmp, "remote project ' quote")
	local := filepath.Join(tmp, "mirror")
	shim := filepath.Join(tmp, "bin")
	state := filepath.Join(tmp, "state")
	for _, p := range []string{localHome, remoteHome, remote, local, shim, state} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	pidfile := filepath.Join(tmp, "ssh.pid")
	script := "#!/bin/sh\nfor arg do command=$arg; done\ncase \"$command\" in *' agent --root '*) printf '%s' \"$$\" > " + transport.Quote(pidfile) + ";; esac\nexport HOME=" + transport.Quote(remoteHome) + "\nexec sh -c \"$command\"\n"
	if err := os.WriteFile(filepath.Join(shim, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", localHome)
	t.Setenv("XDG_STATE_HOME", state)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "config"))
	t.Setenv("PATH", shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	wait := func(fn func() bool) {
		t.Helper()
		end := time.Now().Add(20 * time.Second)
		for time.Now().Before(end) {
			if fn() {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		out, _ := exec.Command(bin, "logs", "sample").CombinedOutput()
		t.Fatalf("CLI condition timed out:\n%s", out)
	}
	write := func(dir, p, s string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, p), []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	read := func(dir, p string) string { b, _ := os.ReadFile(filepath.Join(dir, p)); return string(b) }
	write(remote, "file.go", "initial")
	run("connect", "alias:"+remote, "--local", local, "--name", "sample", "--background", "--debounce", "40ms")
	defer exec.Command(bin, "stop", "sample").Run()
	wait(func() bool { return strings.Contains(run("status"), "Watching") && read(local, "file.go") == "initial" })
	write(remote, "file.go", "remote edit")
	wait(func() bool { return read(local, "file.go") == "remote edit" })
	write(local, "local.go", "local edit")
	wait(func() bool { return read(remote, "local.go") == "local edit" })
	// Kill the persistent SSH subprocess, mutate the remote while disconnected,
	// then verify the supervisor reuses its durable baseline and reconciles.
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 20; n++ {
		write(remote, fmt.Sprintf("offline%02d.go", n), "offline")
	}
	write(remote, "file.go", "after disconnect")
	wait(func() bool {
		return read(local, "offline19.go") == "offline" && read(local, "file.go") == "after disconnect"
	})
	if !strings.Contains(run("logs", "sample"), "Disconnected") {
		t.Fatal("missing reconnect log")
	}
	run("stop", "sample")
	if !strings.Contains(run("status"), "Stopped") {
		t.Fatal("not stopped")
	}
	run("start", "sample")
	wait(func() bool { return strings.Contains(run("status"), "Watching") })
	run("stop", "sample")
	run("remove", "sample")
	if strings.Contains(run("status"), "sample") {
		t.Fatal("not unregistered")
	}
	if read(local, "file.go") != "after disconnect" {
		t.Fatal("remove deleted mirror")
	}
}
