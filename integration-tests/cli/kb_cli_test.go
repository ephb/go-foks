//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives chat-foks's Rust `kb` CLI against a real chatd on the test server.
// Build kb first (cargo build --release -p kb-cli in chat-foks/clients), then:
//
//	KB_BIN=<path to kb> go test -tags chatfoks ./integration-tests/cli -run TestKbCLIAgainstChatd

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	"github.com/stretchr/testify/require"
)

// kb runs the kb CLI against chatd c and returns stdout. It fails the test
// on a non-zero exit unless wantFail is set.
func kbRun(t *testing.T, bin string, c *chatdClient, stdin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"--no-autostart"}, args...)...)
	cmd.Env = append(os.Environ(),
		"KB_CHATD_SOCKET=tcp://"+c.addr,
		"KB_CHATD_TOKEN_FILE="+c.tokenFile,
	)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	require.NoError(t, err, "kb %v failed\nstdout: %s\nstderr: %s", args, out.String(), errOut.String())
	return out.String()
}

func threadBodies(v lcl.RTThreadView) []string {
	var ret []string
	for _, m := range v.Msgs {
		ret = append(ret, string(m.Body))
	}
	return ret
}

func TestKbCLIAgainstChatd(t *testing.T) {
	bin := os.Getenv("KB_BIN")
	if bin == "" {
		t.Skip("set KB_BIN to the kb CLI binary to run this test")
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

	// alice is chatd, signed up over the wire; from here on only kb talks
	// to it (besides this test's own event subscription).
	aliceCfg := newTestAgent(t)
	c := startChatdFresh(t, aliceCfg.cfgFlags)
	c.ok("subscribe", map[string]any{"app": 1}, nil)
	rnd := strings.ToLower(fsRandomString(t, 6))
	aliceName := "alice_" + rnd
	c.signupOverWire(vh.Addr, aliceName, "kb device")
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)

	// kb status
	out := kbRun(t, bin, c, "", "status")
	require.Contains(t, out, aliceName, "kb status: %s", out)

	// kb team create / ls / add / members
	teamName := "kt_" + rnd
	kbRun(t, bin, c, "", "team", "create", teamName)
	merklePoke(t)
	out = kbRun(t, bin, c, "", "team", "ls")
	require.Contains(t, out, teamName, "kb team ls: %s", out)
	kbRun(t, bin, c, "", "team", "add", teamName, bobName)
	merklePoke(t)
	out = kbRun(t, bin, c, "", "team", "members", teamName)
	require.Contains(t, out, bobName, "kb team members: %s", out)

	// kb chat send (argument and stdin) / read in the team's default channel
	kbRun(t, bin, c, "", "chat", "send", teamName, "hello", "team", "from", "kb")
	kbRun(t, bin, c, "piped body from kb\n", "chat", "send", teamName)
	out = kbRun(t, bin, c, "", "chat", "read", teamName, "-n", "10")
	require.Contains(t, out, "hello team from kb", "kb chat read: %s", out)
	require.Contains(t, out, "piped body from kb", "kb chat read: %s", out)

	// kb chat dm, then bob reads it with the foks CLI
	kbRun(t, bin, c, "", "chat", "dm", bobName, "-m", "hi bob, from kb")
	merklePoke(t)
	var thread lcl.RTThreadView
	bob.runCmdToJSON(t, &thread, "rt", "read", "--adhoc", aliceName)
	require.Contains(t, threadBodies(thread), "hi bob, from kb")

	// bob replies; kb chat ls shows the DM, kb chat api reads the reply
	// the way the OpenCode plugin does.
	c.drainEvents()
	bob.runCmd(t, nil, "rt", "send", "--adhoc", aliceName, "hi alice, from bob")
	c.event("inbox_changed", 60*time.Second)

	out = kbRun(t, bin, c, "", "chat", "ls")
	require.Contains(t, out, bobName, "kb chat ls: %s", out)

	dmConv := aliceName + "," + bobName
	req := `{"method":"read","params":{"options":{"channel":{"name":"` + dmConv +
		`","members_type":"impteamnative"},"pagination":{"num":10}}}}`
	out = kbRun(t, bin, c, req, "chat", "api")
	var apiRes struct {
		Result struct {
			Messages []struct {
				Msg struct {
					Content struct {
						Type string `json:"type"`
						Text struct {
							Body string `json:"body"`
						} `json:"text"`
					} `json:"content"`
				} `json:"msg"`
			} `json:"messages"`
		} `json:"result"`
		Error any `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &apiRes), "kb chat api output: %s", out)
	require.Nil(t, apiRes.Error, "kb chat api: %s", out)
	var bodies []string
	for _, m := range apiRes.Result.Messages {
		bodies = append(bodies, m.Msg.Content.Text.Body)
	}
	require.Contains(t, bodies, "hi alice, from bob", "kb chat api read: %s", out)

	// kb chat api send, then bob sees it
	req = `{"method":"send","params":{"options":{"channel":{"name":"` + dmConv +
		`","members_type":"impteamnative"},"message":{"body":"sent via kb chat api"}}}}`
	out = kbRun(t, bin, c, req, "chat", "api")
	require.NotContains(t, out, `"error"`, "kb chat api send: %s", out)
	bob.runCmdToJSON(t, &thread, "rt", "read", "--adhoc", aliceName)
	require.Contains(t, threadBodies(thread), "sent via kb chat api")
}

// kbRunCode runs the kb CLI against chatd c and returns stdout, stderr and
// the exit code, without failing the test on a non-zero exit.
func kbRunCode(t *testing.T, bin string, c *chatdClient, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"--no-autostart"}, args...)...)
	cmd.Env = append(os.Environ(),
		"KB_CHATD_SOCKET=tcp://"+c.addr,
		"KB_CHATD_TOKEN_FILE="+c.tokenFile,
	)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		require.True(t, ok, "kb %v: %v", args, err)
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code
}

// TestKbCLILogin drives kb login/unlock/logout/status against a chatd with
// two accounts, one of them passphrase-protected (FOKS passphrase key
// storage), across a chatd restart.
func TestKbCLILogin(t *testing.T) {
	bin := os.Getenv("KB_BIN")
	if bin == "" {
		t.Skip("set KB_BIN to the kb CLI binary to run this test")
	}
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	cfg := newTestAgentWithOpts(t, agentOpts{defaultKeyEncryptionMode: "passphrase"})
	home := t.TempDir()
	c := startChatdAt(t, cfg.cfgFlags, home)
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice, bob := "alice_"+rnd, "bob_"+rnd
	pp := "kb-pp-" + rnd

	// Nothing yet: exit 5, "kb signup".
	out, _, code := kbRunCode(t, bin, c, "", "status")
	require.Equal(t, 5, code, "kb status on an empty home: %s", out)
	require.Contains(t, out, "run `kb signup`")

	c.signupPassphrase = pp
	c.signupOverWire(vh.Addr, alice, "kb device")
	merklePoke(t)
	c.signupPassphrase = "" // bob has no passphrase
	c.signupOverWire(vh.Addr, bob, "kb device")
	merklePoke(t)

	out = kbRun(t, bin, c, "", "status")
	require.Contains(t, out, "user:    "+bob+"@")

	// kb logout; kb status lists both accounts, exit 5.
	out = kbRun(t, bin, c, "", "logout")
	require.Contains(t, out, "Logged out "+bob)
	out, _, code = kbRunCode(t, bin, c, "", "status")
	require.Equal(t, 5, code, "kb status logged out: %s", out)
	require.Contains(t, out, "logged out")
	require.Contains(t, out, alice+"@")
	require.Contains(t, out, bob+"@")
	_, errOut, code := kbRunCode(t, bin, c, "", "team", "ls")
	require.Equal(t, 5, code)
	require.Contains(t, errOut, "kb login")

	// kb login alice with a wrong passphrase: exit 1, alice active and
	// locked (exit 8).
	_, errOut, code = kbRunCode(t, bin, c, "wrong\n", "login", alice, "--passphrase-stdin")
	require.Equal(t, 1, code, "wrong passphrase: %s", errOut)
	require.Contains(t, errOut, "wrong passphrase")
	require.NotContains(t, errOut, pp)
	out, _, code = kbRunCode(t, bin, c, "", "status")
	require.Equal(t, 8, code, "locked: %s", out)
	require.Contains(t, out, "locked:  passphrase - run `kb unlock`")
	_, errOut, code = kbRunCode(t, bin, c, "", "chat", "ls")
	require.Equal(t, 8, code, "chat ls while locked: %s", errOut)

	// Without --passphrase-stdin and without a terminal, kb refuses.
	_, errOut, code = kbRunCode(t, bin, c, pp+"\n", "unlock")
	require.NotEqual(t, 0, code)
	require.Contains(t, errOut, "--passphrase-stdin")

	out = kbRun(t, bin, c, pp+"\n", "unlock", "--passphrase-stdin")
	require.Contains(t, out, "Unlocked "+alice)
	out = kbRun(t, bin, c, "", "status")
	require.Contains(t, out, "user:    "+alice+"@")
	kbRun(t, bin, c, "", "team", "ls")

	// Switch to bob by number (two accounts listed): no passphrase.
	_, _, code = kbRunCode(t, bin, c, "", "login", "carol_"+rnd)
	require.Equal(t, 1, code)
	out = kbRun(t, bin, c, "", "login", bob)
	require.Contains(t, out, "Logged in as "+bob)

	// Restart chatd: bob (no passphrase) comes back unlocked; alice needs
	// her passphrase again, chosen from the list this time.
	c.stop()
	c = startChatdAt(t, cfg.cfgFlags, home)
	out = kbRun(t, bin, c, "", "status")
	require.Contains(t, out, "user:    "+bob+"@")
	kbRun(t, bin, c, "", "logout")
	// alice is listed first or second; find her number in the prompt.
	_, errOut, _ = kbRunCode(t, bin, c, "", "login")
	n := "1"
	if strings.Index(errOut, alice) > strings.Index(errOut, bob) {
		n = "2"
	}
	out = kbRun(t, bin, c, n+"\n"+pp+"\n", "login", "--passphrase-stdin")
	require.Contains(t, out, "Logged in as "+alice+"@")
	require.Contains(t, out, "(unlocked)")
	out = kbRun(t, bin, c, "", "status")
	require.NotContains(t, out, "locked:")
}

// TestKbCLISignupFlags runs `kb signup` with every answer given as a flag,
// the way chat-foks's deploy smoke check (deploy/hetzner/smoke.sh) does.
// chatd decides the prompt order (this host asks no invite code), so the
// flags answer by prompt kind; nothing is read from the terminal.
func TestKbCLISignupFlags(t *testing.T) {
	bin := os.Getenv("KB_BIN")
	if bin == "" {
		t.Skip("set KB_BIN to the kb CLI binary to run this test")
	}
	vh := vHost(t, 4)
	cfg := newTestAgent(t)
	c := startChatdFresh(t, cfg.cfgFlags)
	invite, err := core.ExportInviteCode(globalTestEnv.G.TestMultiUseInviteCode())
	require.NoError(t, err)
	name := "piped_" + strings.ToLower(fsRandomString(t, 6))
	kbRun(t, bin, c, "", "signup", "--server", string(vh.Addr), "--invite-code", invite,
		"--email", name+"@example.com", "--username", name, "--device-name", "ci-smoke", "--no-passphrase")
	out := kbRun(t, bin, c, "", "status")
	require.Contains(t, out, name, "kb status: %s", out)
}
