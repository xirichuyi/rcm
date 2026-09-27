package transport

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestTargetAndQuoting(t *testing.T) {
	for _, s := range []string{"eul:/root/project", "user@server:/path with ' quotes"} {
		if _, err := Parse(s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range []string{"-oProxyCommand=evil:/x", "host:relative", "host:/", "host\n: /x", "[::1]:/x"} {
		if _, err := Parse(s); err == nil {
			t.Fatal("accepted", s)
		}
	}
	for _, s := range []string{"/a'b", "$(touch /tmp/rcm-should-not-exist)", "spaces ; echo injected"} {
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil || string(out) != s {
			t.Fatal("quote roundtrip", string(out), err)
		}
	}
}
func TestAgentDeploymentThroughSSH(t *testing.T) {
	dir := t.TempDir()
	remoteHome := filepath.Join(dir, "remote home")
	os.MkdirAll(remoteHome, 0700)
	ssh := filepath.Join(dir, "ssh")
	script := "#!/bin/sh\nfor arg do cmd=$arg; done\nexport HOME=" + Quote(remoteHome) + "\nexec sh -c \"$cmd\"\n"
	if err := os.WriteFile(ssh, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	agent := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	agentDir := filepath.Join(dir, "agents")
	os.MkdirAll(agentDir, 0700)
	os.WriteFile(filepath.Join(agentDir, "rcm-"+runtime.GOOS+"-"+runtime.GOARCH), agent, 0700)
	transport := SSH{Target: Target{"alias", "/a project/with'quote"}, AgentDir: agentDir, Executable: ssh, Stderr: os.Stderr}
	path1, err := transport.Agent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path2, err := transport.Agent(context.Background())
	if err != nil || path1 != path2 {
		t.Fatal("cache reuse", err)
	}
	out, err := transport.run(context.Background(), "exec "+path1+" agent --root "+Quote(transport.Target.Root), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), transport.Target.Root) {
		t.Fatal("remote root quoting", string(out))
	}
}
