package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
	"rcm/internal/mirror"
	"rcm/internal/transport"
)

const Version = "0.1.0"

func Run(args []string) error {
	if len(args) == 0 {
		return help()
	}
	switch args[0] {
	case "help", "--help", "-h":
		return help()
	case "version", "--version":
		fmt.Println("rcm", Version, "protocol", mirror.Protocol)
		return nil
	case "agent":
		return agent(args[1:])
	}
	r, err := registry()
	if err != nil {
		return err
	}
	switch args[0] {
	case "connect":
		return connect(r, args[1:])
	case "_run":
		if len(args) != 2 {
			return errors.New("missing project")
		}
		p, err := r.Load(args[1])
		if err != nil {
			return err
		}
		return worker(r, p)
	case "status":
		return status(r)
	case "start", "stop", "remove", "logs":
		if len(args) != 2 {
			return fmt.Errorf("usage: rcm %s PROJECT", args[0])
		}
		p, err := r.Load(args[1])
		if err != nil {
			return err
		}
		switch args[0] {
		case "start":
			return start(r, p)
		case "stop":
			return stop(r, p)
		case "remove":
			if err = stop(r, p); err != nil {
				return err
			}
			err = os.Rename(filepath.Join(r.dir(p.Name), "project.json"), filepath.Join(r.dir(p.Name), "project.removed-"+mirror.ID()+".json"))
			if err == nil {
				fmt.Println("Unregistered; mirror, logs and recovery data retained:", p.Local)
			}
			return err
		case "logs":
			return logs(r, p)
		}
	case "conflicts", "conflict", "diff", "resolve":
		return conflicts(r, args[0], args[1:])
	}
	return fmt.Errorf("unknown command %q; run rcm help", args[0])
}
func help() error {
	fmt.Print(`Remote Code Mirror — remote-primary local source mirror

  rcm connect HOST:/absolute/project [--local DIR] [--name NAME]
              [--background] [--include-git] [--read-only]
              [--debounce 150ms] [--agent-dir DIR] [--verbose]
  rcm status
  rcm start|stop|remove|logs PROJECT
  rcm conflicts [--project NAME]
  rcm conflict|diff PATH [--project NAME]
  rcm resolve PATH --remote|--local [--project NAME]
  rcm version

connect stays in the foreground; Ctrl-C stops it. start runs in background.
remove retains your files. .git is opt-in and never uploaded to the remote.
Recovery copies live in each root's private .rcm-internal directory.
`)
	return nil
}
func flags(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(os.Stderr)
	return f
}

// Permit flags before or after positionals, as shown in the documented CLI.
func parse(f *flag.FlagSet, args []string) error {
	var opts, pos []string
	for n := 0; n < len(args); n++ {
		s := args[n]
		if s == "--" {
			pos = append(pos, args[n+1:]...)
			break
		}
		if strings.HasPrefix(s, "-") && s != "-" {
			opts = append(opts, s)
			name := strings.TrimLeft(strings.SplitN(s, "=", 2)[0], "-")
			v := f.Lookup(name)
			if v != nil && !strings.Contains(s, "=") {
				b, ok := v.Value.(interface{ IsBoolFlag() bool })
				if !ok || !b.IsBoolFlag() {
					n++
					if n >= len(args) {
						return fmt.Errorf("missing value for %s", s)
					}
					opts = append(opts, args[n])
				}
			}
		} else {
			pos = append(pos, s)
		}
	}
	return f.Parse(append(opts, pos...))
}
func globalConfig() (mirror.Config, error) {
	var c mirror.Config
	dir, err := os.UserConfigDir()
	if err != nil {
		return c, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "rcm", "config.yaml"))
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	err = d.Decode(&c)
	if err == io.EOF {
		err = nil
	}
	if err == nil {
		err = c.Validate()
	}
	return c, err
}
func connect(r Registry, args []string) error {
	f := flags("connect")
	local := f.String("local", "", "local mirror")
	name := f.String("name", "", "project name")
	bg := f.Bool("background", false, "detach")
	git := f.Bool("include-git", false, "mirror .git one way")
	ro := f.Bool("read-only", false, "disable uploads")
	debounce := f.String("debounce", "", "coalesce window")
	agents := f.String("agent-dir", "", "development agents")
	verbose := f.Bool("verbose", false, "verbose logs")
	if err := parse(f, args); err != nil {
		return err
	}
	if f.NArg() != 1 {
		return errors.New("usage: rcm connect HOST:/absolute/project [flags]")
	}
	target, err := transport.Parse(f.Arg(0))
	if err != nil {
		return err
	}
	if *name == "" {
		*name = filepath.Base(target.Root)
	}
	if *local == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		*local = filepath.Join(home, "Remote", target.Host, filepath.Base(target.Root))
	}
	abs, err := filepath.Abs(*local)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(abs, 0755); err != nil {
		return err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return err
	}
	if abs == "/" {
		return errors.New("refusing local filesystem root")
	}
	c, err := globalConfig()
	if err != nil {
		return err
	}
	c.IncludeGit = *git
	if *ro {
		b := false
		c.LocalWrite.Enabled = &b
	}
	if *debounce != "" {
		c.Debounce = *debounce
	}
	if err = c.Validate(); err != nil {
		return err
	}
	if *agents != "" {
		*agents, err = filepath.Abs(*agents)
		if err != nil {
			return err
		}
	}
	p := Project{Name: *name, Remote: target.Host + ":" + target.Root, Local: abs, AgentDir: *agents, Config: c, Verbose: *verbose, Created: time.Now()}
	if err = r.Register(p); err != nil {
		return err
	}
	if r.active(p.Name) {
		return errors.New("project already running")
	}
	// Persist options for subsequent `start` calls too.
	if err = atomicJSON(filepath.Join(r.dir(p.Name), "project.json"), p); err != nil {
		return err
	}
	fmt.Printf("RCM\n\nRemote: %s\nLocal:  %s\n\nConnecting...\n", p.Remote, p.Local)
	if *bg {
		return start(r, p)
	}
	return worker(r, p)
}
func agent(args []string) error {
	f := flags("agent")
	root := f.String("root", "", "root")
	if err := f.Parse(args); err != nil {
		return err
	}
	if *root == "" || !filepath.IsAbs(*root) || filepath.Clean(*root) == "/" {
		return errors.New("agent requires --root /absolute/project")
	}
	t, err := mirror.OpenTree(*root)
	if err != nil {
		return err
	}
	defer t.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	go func() { <-ctx.Done(); os.Stdin.Close() }()
	return mirror.RunAgent(ctx, t, mirror.NewWire(os.Stdin, os.Stdout))
}
func worker(r Registry, p Project) error {
	lock, err := os.OpenFile(filepath.Join(r.dir(p.Name), "worker.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return errors.New("project already running")
	}
	t, err := mirror.OpenTree(p.Local)
	if err != nil {
		return err
	}
	defer t.Close()
	rootLock, err := mirror.Lock(t)
	if err != nil {
		return err
	}
	defer rootLock.Close()
	if err = t.Recover(); err != nil {
		return err
	}
	state, err := mirror.LoadState(t)
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(filepath.Join(r.dir(p.Name), "rcm.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer logFile.Close()
	logf := func(format string, a ...any) {
		line := fmt.Sprintf("%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, a...))
		fmt.Fprint(logFile, line)
		fmt.Print(line)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	sock, err := r.controlPath(p.Name)
	if err != nil {
		return err
	}
	_ = os.Remove(sock)
	listener, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer listener.Close()
	_ = os.Chmod(sock, 0600)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			var b [4]byte
			_, err = io.ReadFull(conn, b[:])
			conn.Close()
			if err == nil && string(b[:]) == "stop" {
				cancel()
				return
			}
		}
	}()
	stat := Runtime{PID: os.Getpid()}
	setStatus := func(s string) {
		stat.State = s
		stat.Error = ""
		if err := r.saveRuntime(p.Name, stat); err != nil {
			logf("status persistence failed: %v", err)
		}
	}
	defer setStatus("Stopped")
	target, err := transport.Parse(p.Remote)
	if err != nil {
		return err
	}
	ssh := transport.SSH{Target: target, AgentDir: p.AgentDir, Stderr: io.MultiWriter(os.Stderr, logFile)}
	delay := time.Second
	for {
		if ctx.Err() != nil {
			return nil
		}
		setStatus("Connecting")
		began := time.Now()
		agentPath, e := ssh.Agent(ctx)
		if e == nil {
			logf("SSH connected; remote agent ready")
			stream, openErr := ssh.Open(ctx, agentPath)
			e = openErr
			if e == nil {
				client := mirror.Client{Tree: t, State: state, Config: p.Config, Verbose: p.Verbose, Log: logf, Status: setStatus}
				e = client.Session(ctx, mirror.NewWire(stream.Reader, stream.Writer))
				stream.Close()
			}
		}
		if ctx.Err() != nil {
			return nil
		}
		stat.State = "Disconnected"
		stat.Error = fmt.Sprint(e)
		_ = r.saveRuntime(p.Name, stat)
		logf("Disconnected: %v; retry in %s", e, delay)
		if time.Since(began) > 30*time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if delay < 30*time.Second {
			delay *= 2
			if delay > 30*time.Second {
				delay = 30 * time.Second
			}
		}
	}
}
func start(r Registry, p Project) error {
	if r.active(p.Name) {
		return errors.New("project already running")
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(exe, "_run", p.Name)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// The worker opens its own log; detached stdout is discarded to avoid duplicates.
	errlog, err := os.OpenFile(filepath.Join(r.dir(p.Name), "launcher.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer errlog.Close()
	cmd.Stderr = errlog
	if err = cmd.Start(); err != nil {
		return err
	}
	_ = cmd.Process.Release()
	fmt.Printf("Started %s; rcm status / rcm logs %s\n", p.Name, p.Name)
	return nil
}
func stop(r Registry, p Project) error {
	if !r.active(p.Name) {
		fmt.Println(p.Name, "already stopped")
		return nil
	}
	sock, err := r.controlPath(p.Name)
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		return fmt.Errorf("worker control socket unavailable: %w", err)
	}
	_, err = conn.Write([]byte("stop"))
	conn.Close()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !r.active(p.Name) {
			fmt.Println("Stopped", p.Name)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return errors.New("worker has not stopped yet; no files removed")
}
func status(r Registry) error {
	ps, err := r.List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	defer w.Flush()
	fmt.Fprintln(w, "PROJECT\tREMOTE\tLOCAL\tSTATUS")
	for _, p := range ps {
		s := r.runtime(p.Name)
		if !r.active(p.Name) {
			s.State = "Stopped"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", p.Name, p.Remote, p.Local, s.State)
	}
	return nil
}
func logs(r Registry, p Project) error {
	for _, name := range []string{"rcm.log", "launcher.log"} {
		f, err := os.Open(filepath.Join(r.dir(p.Name), name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err == nil && info.Size() > 64<<10 {
			_, err = f.Seek(-(64 << 10), io.SeekEnd)
		}
		if err == nil {
			_, err = io.Copy(os.Stdout, f)
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
func conflicts(r Registry, command string, args []string) error {
	f := flags(command)
	project := f.String("project", "", "project")
	remote := f.Bool("remote", false, "choose remote")
	local := f.Bool("local", false, "choose local")
	if err := parse(f, args); err != nil {
		return err
	}
	if command != "conflicts" && f.NArg() != 1 {
		return fmt.Errorf("usage: rcm %s PATH [--project NAME]", command)
	}
	ps, err := r.List()
	if err != nil {
		return err
	}
	if *project != "" {
		p, err := r.Load(*project)
		if err != nil {
			return err
		}
		ps = []Project{p}
	}
	if command != "conflicts" && len(ps) != 1 {
		return errors.New("specify --project when multiple projects exist")
	}
	for _, p := range ps {
		t, err := mirror.OpenTree(p.Local)
		if err != nil {
			return err
		}
		defer t.Close()
		s, err := mirror.LoadState(t)
		if err != nil {
			return err
		}
		if command == "conflicts" {
			keys := make([]string, 0, len(s.Conflicts))
			for k := range s.Conflicts {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				cf := s.Conflicts[k]
				fmt.Printf("%s\t%s\tlocal=%s remote=%s\t%s\n", p.Name, k, cf.Local.Entry.Kind, cf.Remote.Entry.Kind, cf.Reason)
			}
			continue
		}
		key := filepath.ToSlash(f.Arg(0))
		if err = mirror.ValidatePath(key); err != nil {
			return err
		}
		cf, ok := s.Conflicts[key]
		if !ok {
			return fmt.Errorf("no conflict for %s", key)
		}
		if command == "conflict" {
			fmt.Printf("%s\nReason: %s\nLocal: %s %s\nRemote: %s %s\nUpdated: %s\n", key, cf.Reason, cf.Local.Entry.Kind, cf.Local.Entry.Hash, cf.Remote.Entry.Kind, cf.Remote.Entry.Hash, cf.Updated.Format(time.RFC3339))
			continue
		}
		now, err := t.Snapshot(key, true)
		if err != nil {
			return err
		}
		defer t.Discard(now)
		if command == "resolve" {
			if *remote == *local {
				return errors.New("choose exactly one of --remote or --local")
			}
			choice := "remote"
			if *local {
				choice = "local"
			}
			err = mirror.QueueResolution(t, mirror.Resolution{Path: key, Choice: choice, Local: now.Entry, Remote: cf.Remote.Entry})
			if err != nil {
				return err
			}
			fmt.Println("Resolution queued; worker rechecks both versions before applying. Use rcm conflicts to verify.")
			continue
		}
		if (now.Entry.Kind != "file" && now.Entry.Kind != "") || (cf.Remote.Entry.Kind != "file" && cf.Remote.Entry.Kind != "") {
			fmt.Printf("local: %+v\nremote: %+v\n", now.Entry, cf.Remote.Entry)
			continue
		}
		a, b := "/dev/null", "/dev/null"
		if cf.Remote.Blob != "" {
			a = filepath.Join(p.Local, cf.Remote.Blob)
		}
		if now.Blob != "" {
			b = filepath.Join(p.Local, now.Blob)
		}
		cmd := exec.Command("diff", "-u", "-L", "remote/"+key, "-L", "local/"+key, a, b)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err = cmd.Run()
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			err = nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
