//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// is not a test that asserts anything: it is an opt-in rig for driving the
// kb-gui desktop client against a real chatd and test server, by hand or by
// UI automation. It is skipped unless KB_GUI_HOLD names a control directory:
//
//	KB_GUI_HOLD=C:\tmp\hold go test -tags chatfoks ./integration-tests/cli \
//	    -run TestKbGUIHold -timeout 90m
//
// It sets up an open-viewership vhost, a second user "bob" (an ordinary CLI
// agent), and starts alice's chatd with an EMPTY home (not signed up) on a
// dev TCP endpoint with a token file. It writes <dir>/info.txt:
//
//	KB_CHATD_SOCKET=tcp://127.0.0.1:port
//	KB_CHATD_TOKEN_FILE=...
//	SERVER=<vhost probe address to type into the signup screen>
//	INVITE=<a multi-use invite code>
//	BOB=<bob's username>
//	CHATD_LOG=...
//
// and then serves commands, one file each in <dir> (the file is deleted when
// the command is taken; results go to <dir>/<name>.out):
//
//	stop           kill alice's chatd (to see the client go offline)
//	start          start it again on the same endpoint and FOKS home
//	bob-dm         body "alice\nmessage": bob sends into the DM with alice
//	bob-team       body "team\nmessage": bob sends into the team's default channel
//	bob-read-dm    body "alice": bob reads the DM; the thread JSON goes to .out
//	bob-read-team  body "team": same for a team's default channel
//	bob-teams      bob lists his team memberships (.out)
//	done           tear everything down and pass
//
// The rig gives up after KB_GUI_HOLD_MINUTES (default 60).

package cli

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/stretchr/testify/require"
)

// holdChatd is alice's chatd process, restartable on the same endpoint and
// FOKS home.
type holdChatd struct {
	t         *testing.T
	bin       string
	args      []string
	env       []string
	addr      string
	tokenFile string
	logPath   string
	proc      *exec.Cmd
}

func (h *holdChatd) start() {
	h.proc = exec.Command(h.bin, h.args...)
	h.proc.Env = h.env
	logFile, err := os.OpenFile(h.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(h.t, err)
	h.proc.Stdout = logFile
	h.proc.Stderr = logFile
	require.NoError(h.t, h.proc.Start())
	go func(p *exec.Cmd, f *os.File) {
		_ = p.Wait()
		_ = f.Close()
	}(h.proc, logFile)
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", h.addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("chatd did not come up on %s: %v", h.addr, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (h *holdChatd) stop() {
	if h.proc != nil && h.proc.Process != nil {
		_ = h.proc.Process.Kill()
		time.Sleep(300 * time.Millisecond)
	}
	h.proc = nil
}

// holdJSON runs a foks command with --json and returns its output, without
// failing the rig when the command fails.
func holdJSON(a *testAgent, args ...string) (string, error) {
	var term terminalUI
	hook := func(m libclient.MetaContext) error {
		m.G().SetUIs(libclient.UIs{Terminal: &term})
		return nil
	}
	err := a.runCmdErr(hook, append([]string{"--json"}, args...)...)
	return string(term.Bytes()), err
}

func TestKbGUIHold(t *testing.T) {
	dir := os.Getenv("KB_GUI_HOLD")
	if dir == "" {
		t.Skip("set KB_GUI_HOLD to a control directory to run the kb-gui rig")
	}
	require.NoError(t, os.MkdirAll(dir, 0o700))
	minutes := 60
	if v := os.Getenv("KB_GUI_HOLD_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		require.NoError(t, err)
		minutes = n
	}

	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	bob := newTestAgent(t)
	bob.runAgent(t)
	defer bob.stop(t)
	newUserWithAgentAtVHost(t, bob, 4)
	merklePoke(t)
	merklePoke(t)
	setVHostOpenViewership(t, bob)
	bobName := string(getActiveUser(t, bob).Info.Username.NameUtf8)

	invite, err := core.ExportInviteCode(globalTestEnv.G.TestMultiUseInviteCode())
	require.NoError(t, err)

	// alice: chatd on an empty home of its own (as startChatdFresh), but no
	// client here: kb-gui does the hello, signup and everything else.
	aliceCfg := newTestAgent(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())
	work := t.TempDir()
	appData := filepath.Join(work, "localappdata")
	require.NoError(t, os.MkdirAll(appData, 0o700))
	h := &holdChatd{
		t:         t,
		bin:       buildChatd(t),
		addr:      addr,
		tokenFile: filepath.Join(work, "chatd.token"),
		logPath:   filepath.Join(work, "chatd.log"),
		env:       append(os.Environ(), "LocalAppData="+appData, "LOCALAPPDATA="+appData),
	}
	h.args = append([]string{"-insecure-dev-tcp", "-socket", "tcp://" + addr,
		"-token-file", h.tokenFile, "--"}, aliceCfg.cfgFlags...)
	h.start()
	defer h.stop()

	info := fmt.Sprintf("KB_CHATD_SOCKET=tcp://%s\nKB_CHATD_TOKEN_FILE=%s\nSERVER=%s\nINVITE=%s\nBOB=%s\nCHATD_LOG=%s\n",
		addr, h.tokenFile, vh.Addr, invite, bobName, h.logPath)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "info.txt"), []byte(info), 0o600))
	t.Logf("kb-gui rig ready:\n%s", info)

	// take returns the body of a command file and deletes it.
	take := func(name string) (string, bool) {
		p := filepath.Join(dir, name)
		b, err := os.ReadFile(p)
		if err != nil {
			return "", false
		}
		_ = os.Remove(p)
		return strings.TrimSpace(string(b)), true
	}
	result := func(name string, out string) {
		_ = os.WriteFile(filepath.Join(dir, name+".out"), []byte(out), 0o600)
		t.Logf("%s: %s", name, out)
	}
	two := func(body string) (string, string) {
		first, rest, _ := strings.Cut(body, "\n")
		return strings.TrimSpace(first), strings.TrimSpace(rest)
	}

	deadline := time.Now().Add(time.Duration(minutes) * time.Minute)
	for time.Now().Before(deadline) {
		if _, ok := take("done"); ok {
			result("done", "ok")
			return
		}
		if _, ok := take("stop"); ok {
			h.stop()
			result("stop", "stopped")
		}
		if _, ok := take("start"); ok {
			h.start()
			result("start", "started")
		}
		if body, ok := take("bob-dm"); ok {
			alice, msg := two(body)
			err := bob.runCmdErr(nil, "rt", "send", "--adhoc", alice, msg)
			result("bob-dm", fmt.Sprintf("err=%v", err))
		}
		if body, ok := take("bob-team"); ok {
			team, msg := two(body)
			err := bob.runCmdErr(nil, "rt", "send", "-t", team, msg)
			result("bob-team", fmt.Sprintf("err=%v", err))
		}
		if body, ok := take("bob-read-dm"); ok {
			out, err := holdJSON(bob, "rt", "read", "--adhoc", body)
			result("bob-read-dm", fmt.Sprintf("err=%v\n%s", err, out))
		}
		if body, ok := take("bob-read-team"); ok {
			out, err := holdJSON(bob, "rt", "read", "-t", body)
			result("bob-read-team", fmt.Sprintf("err=%v\n%s", err, out))
		}
		if _, ok := take("bob-teams"); ok {
			out, err := holdJSON(bob, "team", "list-memberships")
			result("bob-teams", fmt.Sprintf("err=%v\n%s", err, out))
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("kb-gui rig timed out after %d minutes", minutes)
}
