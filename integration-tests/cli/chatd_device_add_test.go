//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo:
// adding a device over chatd's wire. A chatd that is signed in vouches for
// a chatd on an empty home (device_assist_* and device_login_*); each side
// shows a code and the test carries one of them across, once in each
// direction. With KB_BIN the second round is `kb device add` and
// `kb login --with-device`.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdDeviceAdd

package cli

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ephb/chat-foks/backend/shim/wire"
	"github.com/stretchr/testify/require"
)

type kexStart struct {
	Session   string `json:"session"`
	Code      string `json:"code"`
	Username  string `json:"username"`
	Host      string `json:"host"`
	TimeoutMs int64  `json:"timeout_ms"`
}

type kexDone struct {
	Session string          `json:"session"`
	OK      bool            `json:"ok"`
	Error   *wire.WireError `json:"error"`
}

func (c *chatdClient) kexDone(event string) kexDone {
	c.t.Helper()
	var d kexDone
	require.NoError(c.t, json.Unmarshal(c.event(event, 2*time.Minute).Data, &d))
	return d
}

func teamNames(c *chatdClient) []string {
	var teams struct {
		Teams []struct {
			Team map[string]any `json:"team"`
		} `json:"teams"`
	}
	c.ok("list_teams", nil, &teams)
	var ret []string
	for _, tm := range teams.Teams {
		if s, ok := tm.Team["value"].(string); ok {
			ret = append(ret, s)
		}
	}
	return ret
}

func deviceNames(ds []wireDevice) []string {
	var ret []string
	for _, d := range ds {
		ret = append(ret, d.Name)
	}
	return ret
}

// wholeThread pages back from the newest message to the first one and
// returns every body.
func wholeThread(c *chatdClient, team, channel map[string]any) []string {
	c.t.Helper()
	var ret []string
	before := 0
	for i := 0; i < 20; i++ {
		var v struct {
			Messages []struct {
				Seq  int    `json:"seq"`
				Body string `json:"body"`
			} `json:"messages"`
			AtBeginning bool `json:"at_beginning"`
		}
		c.ok("get_thread_view", map[string]any{"team": team, "app": 1, "channel": channel, "before": before, "num": 50}, &v)
		for _, m := range v.Messages {
			ret = append(ret, m.Body)
			if before == 0 || m.Seq < before {
				before = m.Seq
			}
		}
		if v.AtBeginning || len(v.Messages) == 0 {
			return ret
		}
	}
	c.t.Fatalf("the thread has no beginning: %v", ret)
	return nil
}

// noCodeInLog fails if a chatd log under dir has one of the codes.
func noCodeInLog(t *testing.T, dir string, codes ...string) {
	t.Helper()
	b, err := os.ReadFile(dir + "/chatd.log")
	require.NoError(t, err)
	for _, code := range codes {
		require.NotEmpty(t, code)
		require.NotContains(t, string(b), code, "a code is in %s/chatd.log", dir)
	}
}

func TestChatdDeviceAdd(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)
	kbBin := os.Getenv("KB_BIN")

	// A: the desk, signed up, with a team and a message in it.
	cfgA := newTestAgent(t)
	homeA := t.TempDir()
	a := startChatdAt(t, cfgA.cfgFlags, homeA)
	a.ok("subscribe", map[string]any{"app": 1}, nil)
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice := "alice_" + rnd
	a.signupOverWire(vh.Addr, alice, "desk")
	a.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	teamName := "Kx_" + rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	a.ok("create_team", map[string]any{"name": teamName}, &created)
	team := created.Team
	merklePoke(t)
	deflt := map[string]any{"kind": "default"}
	a.ok("send", map[string]any{"team": team, "app": 1, "channel": deflt, "body": "written on the desk"}, nil)
	merklePoke(t)

	// B: a laptop with an empty home.
	cfgB := newTestAgent(t)
	homeB := t.TempDir()
	b := startChatdAt(t, cfgB.cfgFlags, homeB)
	b.ok("subscribe", map[string]any{"app": 1}, nil)
	require.Nil(t, b.status().User)

	// Nobody is signed in on B: it can't vouch for anyone.
	f := b.call("device_assist_start", nil)
	require.False(t, f.OK)
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)

	// The server has no such account.
	f = b.call("device_login_start", map[string]any{"server": string(vh.Addr), "username": "nobody_" + rnd, "device": "laptop"})
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	t.Logf("unknown username: %s", f.Error.Message)

	// A starts, is refused a second session, cancels; the next start is a
	// new session with a new code.
	var cancelled kexStart
	a.ok("device_assist_start", nil, &cancelled)
	require.Equal(t, alice, cancelled.Username)
	f = a.call("device_assist_start", nil)
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	a.ok("device_assist_cancel", map[string]any{"session": cancelled.Session}, nil)
	d := a.kexDone("device_assist_done")
	require.False(t, d.OK)
	require.Equal(t, "rejected", d.Error.Code)
	require.Contains(t, d.Error.Message, "cancelled")

	// A name the account already has: both sides are told, after the code
	// was accepted.
	var onA, onB kexStart
	a.ok("device_assist_start", nil, &onA)
	b.ok("device_login_start", map[string]any{"server": string(vh.Addr), "username": alice, "device": "desk"}, &onB)
	a.ok("device_assist_answer", map[string]any{"session": onA.Session, "code": onB.Code}, nil)
	d = a.kexDone("device_assist_done")
	require.False(t, d.OK)
	require.Equal(t, "bad_args", d.Error.Code, "%+v", d.Error)
	require.Contains(t, d.Error.Message, "already has that name")
	d = b.kexDone("device_login_done")
	require.False(t, d.OK)
	require.Equal(t, "bad_args", d.Error.Code, "%+v", d.Error)
	require.Contains(t, d.Error.Message, "already has that name")
	require.Nil(t, b.status().User)
	require.Len(t, listDevices(a), 1)

	// The new device gives up. Nothing tells the desk: the server still
	// holds the laptop's first message, so its code is even accepted, and
	// the desk then waits for a new device that is gone, until it is
	// cancelled or runs out of time. No key is added.
	a.ok("device_assist_start", nil, &onA)
	b.ok("device_login_start", map[string]any{"server": string(vh.Addr), "username": alice, "device": "laptop"}, &onB)
	b.ok("device_login_cancel", map[string]any{"session": onB.Session}, nil)
	d = b.kexDone("device_login_done")
	require.False(t, d.OK)
	require.Contains(t, d.Error.Message, "cancelled")
	a.ok("device_assist_answer", map[string]any{"session": onA.Session, "code": onB.Code}, nil)
	select {
	case ev := <-a.events:
		require.NotEqual(t, "device_assist_done", ev.Event, "%s", ev.Data)
	case <-time.After(3 * time.Second):
	}
	a.ok("device_assist_cancel", map[string]any{"session": onA.Session}, nil)
	d = a.kexDone("device_assist_done")
	require.False(t, d.OK)
	require.Contains(t, d.Error.Message, "cancelled")
	require.Nil(t, b.status().User)
	merklePoke(t)
	require.Len(t, listDevices(a), 1)

	// Round 1, over the wire: the desk types the laptop's code.
	a.ok("device_assist_start", nil, &onA)
	require.NotEqual(t, cancelled.Session, onA.Session)
	require.NotEqual(t, cancelled.Code, onA.Code)
	b.ok("device_login_start", map[string]any{"server": string(vh.Addr), "username": alice, "device": "laptop"}, &onB)
	t.Logf("codes have %d and %d words", len(strings.Fields(onA.Code)), len(strings.Fields(onB.Code)))
	require.GreaterOrEqual(t, len(strings.Fields(onA.Code)), 4)
	require.NotEqual(t, onA.Code, onB.Code)
	require.Positive(t, onB.TimeoutMs)

	// A wrong code first: the cancelled session's, and something that is
	// no code at all. The session goes on.
	for _, wrong := range []string{cancelled.Code, "this is not a code"} {
		f = a.call("device_assist_answer", map[string]any{"session": onA.Session, "code": wrong})
		require.False(t, f.OK)
		require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
		require.NotContains(t, f.Error.Message, wrong)
	}
	a.ok("device_assist_answer", map[string]any{"session": onA.Session, "code": onB.Code}, nil)
	d = a.kexDone("device_assist_done")
	require.True(t, d.OK, "%+v", d.Error)
	d = b.kexDone("device_login_done")
	require.True(t, d.OK, "%+v", d.Error)
	b.event("user_changed", 30*time.Second)

	st := b.status()
	require.NotNil(t, st.User)
	require.Equal(t, alice, st.User.Username)
	require.True(t, st.User.Unlocked)
	require.Equal(t, "laptop", st.User.Device)
	merklePoke(t)
	merklePoke(t)
	ds := listDevices(b)
	t.Logf("devices seen from the laptop: %+v", ds)
	require.ElementsMatch(t, []string{"desk", "laptop"}, deviceNames(ds))
	for _, dev := range ds {
		require.Equal(t, dev.Name == "laptop", dev.ThisDevice, "%+v", dev)
		require.Equal(t, "computer", dev.Type)
	}
	require.Len(t, listDevices(a), 2)
	require.Contains(t, teamNames(b), teamName)
	require.Equal(t, []string{"written on the desk"}, wholeThread(b, team, deflt))
	// And the other way round.
	b.ok("send", map[string]any{"team": team, "app": 1, "channel": deflt, "body": "written on the laptop"}, nil)
	merklePoke(t)
	require.Eventually(t, func() bool { return len(threadMsgs(a, team, deflt)) == 2 }, time.Minute, 500*time.Millisecond)
	// It has a key of its own: it is still signed in after a restart.
	b.stop()
	b = startChatdAt(t, cfgB.cfgFlags, homeB)
	st = b.status()
	require.NotNil(t, st.User)
	require.Equal(t, alice, st.User.Username)
	require.True(t, st.User.Unlocked)
	noCodeInLog(t, homeA, onA.Code, onB.Code, cancelled.Code)
	noCodeInLog(t, homeB, onA.Code, onB.Code)

	if kbBin == "" {
		t.Log("KB_BIN is not set: the kb round is skipped")
		return
	}
	out := kbRun(t, kbBin, b, "", "device", "ls")
	t.Logf("kb device ls on the laptop:\n%s", out)
	require.Contains(t, out, "desk")
	require.Contains(t, out, "laptop")
	require.Equal(t, 2, strings.Count(out, "computer"), "two devices: %s", out)

	// Round 2, with kb: the new device (a tablet) types the laptop's code.
	cfgC := newTestAgent(t)
	c := startChatdAt(t, cfgC.cfgFlags, t.TempDir())
	add := exec.Command(kbBin, "--no-autostart", "device", "add")
	add.Env = append(os.Environ(), "KB_CHATD_SOCKET=tcp://"+b.addr, "KB_CHATD_TOKEN_FILE="+b.tokenFile)
	addIn, err := add.StdinPipe() // held open: nothing is typed on the laptop
	require.NoError(t, err)
	defer addIn.Close()
	addOut, err := add.StdoutPipe()
	require.NoError(t, err)
	var addErr strings.Builder
	add.Stderr = &addErr
	require.NoError(t, add.Start())
	defer func() { _ = add.Process.Kill() }()
	lines := bufio.NewReader(addOut)
	var seen strings.Builder
	code := ""
	for code == "" {
		line, err := lines.ReadString('\n')
		seen.WriteString(line)
		require.NoError(t, err, "kb device add ended early\nstdout: %s\nstderr: %s", seen.String(), addErr.String())
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "This device's code:"); ok {
			code = strings.TrimSpace(rest)
		}
	}
	require.GreaterOrEqual(t, len(strings.Fields(code)), 4, "kb device add: %s", seen.String())

	// A wrong code first (asked again), then the right one. The wrong one
	// is round 1's laptop code: a new device's, so not one that a new
	// device can use. (The desk's unused code from round 1 would be
	// accepted, see the cancelled login above.)
	out, errOut, exit := kbRunCode(t, kbBin, c, onB.Code+"\n"+code+"\n", "login", alice, "--with-device",
		"--server", string(vh.Addr), "--device", "tablet")
	t.Logf("kb login --with-device (exit %d):\n%s\n%s", exit, out, errOut)
	require.Equal(t, 0, exit)
	require.Contains(t, out, "This device's code: ")
	require.Contains(t, errOut, "That code is not right")
	require.Contains(t, errOut, "Code accepted")
	require.Contains(t, out, "Signed in as "+alice+"@")
	require.Contains(t, out, "(tablet)")
	require.NotContains(t, out+errOut, code, "a typed code is not repeated")

	rest, err := io.ReadAll(lines)
	require.NoError(t, err)
	seen.Write(rest)
	require.NoError(t, add.Wait(), "kb device add\nstdout: %s\nstderr: %s", seen.String(), addErr.String())
	t.Logf("kb device add:\n%s", seen.String())
	require.Contains(t, seen.String(), "Added the new device")

	merklePoke(t)
	merklePoke(t)
	out = kbRun(t, kbBin, c, "", "device", "ls")
	t.Logf("kb device ls on the tablet:\n%s", out)
	for _, name := range []string{"desk", "laptop", "tablet"} {
		require.Contains(t, out, name)
	}
	require.Equal(t, 3, strings.Count(out, "computer"), "three devices: %s", out)
	out = kbRun(t, kbBin, c, "", "team", "ls")
	require.Contains(t, out, teamName)
	// The tablet reads what both older devices wrote, on its first page
	// of the thread: FOKS has only the newest message in the new device's
	// cache and answered just that one, with at_beginning false, until
	// chatd filled the page from the server (chat-foks notes/15, 7).
	var first struct {
		Messages []struct {
			Body string `json:"body"`
		} `json:"messages"`
		AtBeginning bool `json:"at_beginning"`
	}
	c.ok("get_thread_view", map[string]any{"team": team, "app": 1, "channel": deflt, "before": 0, "num": 50}, &first)
	var firstPage []string
	for _, m := range first.Messages {
		firstPage = append(firstPage, m.Body)
	}
	t.Logf("the tablet's first page: %q, at_beginning %v", firstPage, first.AtBeginning)
	require.ElementsMatch(t, []string{"written on the desk", "written on the laptop"}, firstPage,
		"the first page of the thread on a new device")
	require.True(t, first.AtBeginning, "the first page reaches the beginning")
	require.ElementsMatch(t, []string{"written on the desk", "written on the laptop"}, wholeThread(c, team, deflt))
	out = kbRun(t, kbBin, c, "", "chat", "read", teamName, "-n", "10", "--peek")
	t.Logf("kb chat read on the tablet:\n%s", out)
	require.Contains(t, out, "written on the laptop")
	require.Contains(t, out, "written on the desk")
}
