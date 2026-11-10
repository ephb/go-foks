//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives a real chatd subprocess through WIRE.md version 3: signup over the
// wire, teams, DMs and pushed events. Run from a chat-foks checkout:
//
//	go test -tags chatfoks ./integration-tests/cli -run TestChatdV3

package cli

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"

	"github.com/terra/kebase-fork/backend/shim/wire"
)

// chatdFrame is either a response (ID set) or an event (ID null).
type chatdFrame struct {
	ID     *int64          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result"`
	Error  *wire.WireError `json:"error"`
	Event  string          `json:"event"`
	Data   json.RawMessage `json:"data"`
}

type chatdClient struct {
	t      *testing.T
	conn   net.Conn
	nextID int64
	resps  chan chatdFrame
	events chan chatdFrame
}

func buildChatd(t *testing.T) string {
	if bin := os.Getenv("CHATD_TEST_BIN"); bin != "" {
		return bin
	}
	bin := filepath.Join(t.TempDir(), "chatd.exe")
	out, err := exec.Command("go", "build", "-ldflags=-s -w", "-o", bin,
		"github.com/terra/kebase-fork/backend/shim/cmd/chatd").CombinedOutput()
	require.NoError(t, err, "building chatd failed: %s", string(out))
	return bin
}

// startChatdFresh starts chatd on an empty FOKS home of its own: a private
// LocalAppData (FOKS ignores --home on Windows, see base_test.go TestMain),
// so it shares nothing with the in-process test agents. It returns an
// authenticated v3 client.
func startChatdFresh(t *testing.T, cfgFlags []string) *chatdClient {
	bin := buildChatd(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := probe.Addr().String()
	require.NoError(t, probe.Close())

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "chatd.token")
	appData := filepath.Join(dir, "localappdata")
	require.NoError(t, os.MkdirAll(appData, 0o700))

	args := append([]string{"-insecure-dev-tcp", "-socket", "tcp://" + addr,
		"-token-file", tokenFile, "--"}, cfgFlags...)
	proc := exec.Command(bin, args...)
	proc.Env = append(os.Environ(), "LocalAppData="+appData, "LOCALAPPDATA="+appData)
	logFile, err := os.Create(filepath.Join(dir, "chatd.log"))
	require.NoError(t, err)
	proc.Stdout = logFile
	proc.Stderr = logFile
	require.NoError(t, proc.Start())
	t.Cleanup(func() {
		_ = proc.Process.Kill()
		_, _ = proc.Process.Wait()
		_ = logFile.Close()
		if t.Failed() {
			if b, err := os.ReadFile(logFile.Name()); err == nil {
				t.Logf("chatd log:\n%s", b)
			}
		}
	})

	var conn net.Conn
	deadline := time.Now().Add(30 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chatd did not come up on %s: %v", addr, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Cleanup(func() { _ = conn.Close() })

	c := &chatdClient{t: t, conn: conn, resps: make(chan chatdFrame, 16), events: make(chan chatdFrame, 256)}
	go func() {
		for {
			b, err := wire.ReadFrame(conn)
			if err != nil {
				close(c.resps)
				return
			}
			var f chatdFrame
			if err := json.Unmarshal(b, &f); err != nil {
				continue
			}
			if f.ID == nil {
				select {
				case c.events <- f:
				default: // a test that ignores events must not wedge the reader
				}
			} else {
				c.resps <- f
			}
		}
	}()

	token, err := os.ReadFile(tokenFile)
	require.NoError(t, err)
	c.ok("hello", map[string]any{"token": string(token), "client": "chatd-v3-test/0"}, nil)
	return c
}

func (c *chatdClient) call(op string, params any) chatdFrame {
	c.t.Helper()
	c.nextID++
	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		require.NoError(c.t, err)
		raw = b
	}
	b, err := json.Marshal(wire.Request{V: 3, ID: c.nextID, Op: op, Params: raw})
	require.NoError(c.t, err)
	require.NoError(c.t, wire.WriteFrame(c.conn, b))
	select {
	case f, ok := <-c.resps:
		require.True(c.t, ok, "%s: connection closed", op)
		require.Equal(c.t, c.nextID, *f.ID)
		return f
	case <-time.After(2 * time.Minute):
		c.t.Fatalf("%s: no response", op)
	}
	return chatdFrame{}
}

func (c *chatdClient) ok(op string, params any, out any) {
	c.t.Helper()
	f := c.call(op, params)
	require.True(c.t, f.OK, "%s failed: %+v", op, f.Error)
	if out != nil {
		require.NoError(c.t, json.Unmarshal(f.Result, out), "%s: %s", op, f.Result)
	}
}

func (c *chatdClient) event(name string, timeout time.Duration) chatdFrame {
	c.t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case f := <-c.events:
			if f.Event == name {
				return f
			}
		case <-deadline:
			c.t.Fatalf("no %s event within %v", name, timeout)
		}
	}
}

func (c *chatdClient) drainEvents() {
	for {
		select {
		case <-c.events:
		default:
			return
		}
	}
}

// signupOverWire answers every signup_prompt the way mockSignupUI does
// (integration-tests/cli/signup_test.go), with values sent over the socket.
func (c *chatdClient) signupOverWire(server proto.TCPAddr, username, device string) {
	t := c.t
	var start struct {
		Session string `json:"session"`
	}
	c.ok("signup_start", map[string]any{}, &start)

	// A second session while one runs is refused.
	f := c.call("signup_start", map[string]any{})
	require.False(t, f.OK)
	require.Equal(t, "rejected", f.Error.Code)

	invite, err := core.ExportInviteCode(globalTestEnv.G.TestMultiUseInviteCode())
	require.NoError(t, err)

	var kinds []string
	for {
		var ev chatdFrame
		select {
		case ev = <-c.events:
		case <-time.After(2 * time.Minute):
			t.Fatalf("signup stalled after prompts %v", kinds)
		}
		switch ev.Event {
		case "signup_prompt":
			var p struct {
				Session string  `json:"session"`
				Prompt  int64   `json:"prompt"`
				Kind    string  `json:"kind"`
				Attempt int     `json:"attempt"`
				Confirm bool    `json:"confirm"`
				Default *string `json:"default"`
			}
			require.NoError(t, json.Unmarshal(ev.Data, &p))
			require.Equal(t, start.Session, p.Session)
			require.Equal(t, 1, p.Attempt, "prompt %s was retried", p.Kind)
			kinds = append(kinds, p.Kind)
			ans := map[string]any{"session": p.Session, "prompt": p.Prompt}
			switch p.Kind {
			case "server":
				ans["value"] = string(server)
			case "invite_code":
				ans["value"] = invite
			case "email":
				ans["value"] = "a" + strings.ToLower(fsRandomString(t, 8)) + "@example.com"
			case "username":
				ans["value"] = username
			case "device_name":
				ans["value"] = device
			case "passphrase":
				ans["value"] = "" // no passphrase, like mockSignupUI
			case "pick_existing_user":
				ans["choice"] = -1
			default:
				t.Fatalf("unexpected prompt kind %q", p.Kind)
			}
			c.ok("signup_answer", ans, nil)
		case "signup_done":
			var d struct {
				OK    bool            `json:"ok"`
				Error *wire.WireError `json:"error"`
			}
			require.NoError(t, json.Unmarshal(ev.Data, &d))
			require.True(t, d.OK, "signup failed after prompts %v: %+v", kinds, d.Error)
			require.Contains(t, kinds, "server")
			require.Contains(t, kinds, "invite_code")
			require.Contains(t, kinds, "username")
			require.Contains(t, kinds, "device_name")
			t.Logf("signup prompts: %v", kinds)
			return
		}
	}
}

// TestChatdV3SignupTeamsAndDM runs WIRE.md v3 end to end against a real
// chatd process that starts with an empty FOKS home:
//
//  1. subscribe before any user exists (connectivity: offline);
//  2. sign up over the wire (signup_start / signup_prompt / signup_answer),
//     which must end in user_changed and an unlocked user in status;
//  3. create_team, list_teams, a taken name (bad_args), team_add_member
//     with a second user (bob, a CLI agent), team_members;
//  4. create_team_invite (accepted by bob with `foks team accept`), and
//     accept_team_invite for an invite bob made;
//  5. start_dm with bob (idempotent), send into it, and receive
//     inbox_changed after bob replies from his own agent.
//
// Everyone is on vhost 4 with open viewership: ad-hoc teams and direct
// `team add` by username need it (see rt_test.go/team_test.go).
func TestChatdV3SignupTeamsAndDM(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()

	vh := vHost(t, 4)

	// bob: an ordinary CLI agent on vhost 4, which he switches to open
	// viewership.
	bob := newTestAgent(t)
	bob.runAgent(t)
	defer bob.stop(t)
	newUserWithAgentAtVHost(t, bob, 4)
	merklePoke(t)
	merklePoke(t)
	setVHostOpenViewership(t, bob)
	bobName := string(getActiveUser(t, bob).Info.Username.NameUtf8)

	// alice: chatd with an empty home. Only the test-rig flags (probe,
	// CAs, DNS aliases) come from a test agent; nothing is signed up there.
	aliceCfg := newTestAgent(t)
	c := startChatdFresh(t, aliceCfg.cfgFlags)

	var st struct {
		WireVersion int `json:"wire_version"`
		User        *struct {
			Username string `json:"username"`
			Host     string `json:"host"`
			Device   string `json:"device"`
			Unlocked bool   `json:"unlocked"`
		} `json:"user"`
		Online bool `json:"online"`
	}
	c.ok("status", nil, &st)
	require.Equal(t, 3, st.WireVersion)
	require.Nil(t, st.User)
	require.False(t, st.Online)

	c.ok("subscribe", map[string]any{"app": 1}, nil)
	ev := c.event("connectivity", 10*time.Second)
	require.JSONEq(t, `{"online":false}`, string(ev.Data))

	rnd := strings.ToLower(fsRandomString(t, 6))
	aliceName := "alice_" + rnd
	start := time.Now()
	c.signupOverWire(vh.Addr, aliceName, "chatd device")
	t.Logf("signup over the wire took %v", time.Since(start))
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)

	c.ok("status", nil, &st)
	require.NotNil(t, st.User)
	require.Equal(t, aliceName, st.User.Username)
	require.True(t, st.User.Unlocked)
	require.Equal(t, "chatd device", st.User.Device)

	// The poll loop starts once the user exists.
	ev = c.event("connectivity", 30*time.Second)
	require.JSONEq(t, `{"online":true}`, string(ev.Data))

	// --- teams ---
	teamName := "t_" + rnd
	var created struct {
		Team map[string]string `json:"team"`
	}
	c.ok("create_team", map[string]any{"name": teamName}, &created)
	require.Equal(t, map[string]string{"kind": "named", "value": teamName}, created.Team)
	merklePoke(t)

	f := c.call("create_team", map[string]any{"name": teamName})
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code, "taken name: %+v", f.Error)

	var teams struct {
		Teams []struct {
			Team map[string]string `json:"team"`
			Role map[string]any    `json:"role"`
		} `json:"teams"`
	}
	c.ok("list_teams", nil, &teams)
	found := false
	for _, tm := range teams.Teams {
		if tm.Team["value"] == teamName {
			found = true
			require.Equal(t, "named", tm.Team["kind"])
			require.Equal(t, "owner", tm.Role["kind"])
		}
	}
	require.True(t, found, "list_teams: %+v", teams)

	teamParam := created.Team
	c.ok("team_add_member", map[string]any{"team": teamParam, "users": []string{bobName}}, nil)
	merklePoke(t)

	var members struct {
		Members []struct {
			Name   string         `json:"name"`
			IsTeam bool           `json:"is_team"`
			Role   map[string]any `json:"role"`
		} `json:"members"`
	}
	c.ok("team_members", map[string]any{"team": teamParam}, &members)
	names := map[string]string{}
	for _, m := range members.Members {
		names[m.Name] = m.Role["kind"].(string)
	}
	require.Equal(t, map[string]string{aliceName: "owner", bobName: "member"}, names)

	// --- invites ---
	var inv struct {
		Code string `json:"code"`
	}
	c.ok("create_team_invite", map[string]any{"team": teamParam}, &inv)
	require.NotEmpty(t, inv.Code)
	_, err := team.ImportTeamInvite(inv.Code)
	require.NoError(t, err, "the code must be what `foks team accept` takes")

	bobTeam := "bt_" + rnd
	var bobCreate lcl.TeamCreateRes
	bob.runCmdToJSON(t, &bobCreate, "team", "create", bobTeam)
	merklePoke(t)
	var bobInvite proto.TeamInvite
	bob.runCmdToJSON(t, &bobInvite, "team", "invite", bobTeam)
	bobInviteStr, err := team.ExportTeamInvite(bobInvite)
	require.NoError(t, err)
	var accepted struct {
		Team map[string]string `json:"team"`
	}
	c.ok("accept_team_invite", map[string]any{"code": bobInviteStr}, &accepted)
	require.Equal(t, map[string]string{"kind": "named", "value": bobTeam}, accepted.Team)
	var inbox lcl.TeamInbox
	bob.runCmdToJSON(t, &inbox, "team", "inbox", bobTeam)
	require.Len(t, inbox.Rows, 1, "alice's join request should be in bob's team inbox")

	// --- DMs ---
	var dm struct {
		Team    map[string]string `json:"team"`
		Channel map[string]string `json:"channel"`
	}
	c.ok("start_dm", map[string]any{"users": []string{bobName}}, &dm)
	wantDM, err := team.NamesToAdhHocCanonicalString([]proto.NameUtf8{proto.NameUtf8(bobName)}, proto.NameUtf8(aliceName))
	require.NoError(t, err)
	require.Equal(t, map[string]string{"kind": "adhoc", "value": string(wantDM)}, dm.Team)
	require.Equal(t, map[string]string{"kind": "default"}, dm.Channel)
	merklePoke(t)

	var dm2 struct {
		Team map[string]string `json:"team"`
	}
	c.ok("start_dm", map[string]any{"users": []string{bobName}}, &dm2)
	require.Equal(t, dm.Team, dm2.Team, "start_dm must be idempotent")

	var sent struct {
		Outcome string `json:"outcome"`
	}
	c.ok("send", map[string]any{"team": dm.Team, "app": 1, "channel": dm.Channel, "body": "hi bob, from chatd"}, &sent)
	require.Equal(t, "acked", sent.Outcome)

	var thread lcl.RTThreadView
	bob.runCmdToJSON(t, &thread, "rt", "read", "--adhoc", aliceName)
	require.Len(t, thread.Msgs, 1)
	require.Equal(t, "hi bob, from chatd", string(thread.Msgs[0].Body))

	// --- events: bob replies, chatd's poll loop pushes inbox_changed ---
	c.event("inbox_changed", 60*time.Second) // alice's own send bumps her inbox too
	c.drainEvents()
	bob.runCmd(t, nil, "rt", "send", "--adhoc", aliceName, "hi alice, from bob")
	evStart := time.Now()
	ev = c.event("inbox_changed", 60*time.Second)
	t.Logf("inbox_changed arrived %v after bob's send: %s", time.Since(evStart), ev.Data)

	var view struct {
		Messages []struct {
			Body       string  `json:"body"`
			SenderName *string `json:"sender_name"`
		} `json:"messages"`
	}
	c.ok("get_thread_view", map[string]any{"team": dm.Team, "app": 1, "channel": dm.Channel, "before": 0, "num": 10}, &view)
	require.Len(t, view.Messages, 2)
	require.Equal(t, "hi alice, from bob", view.Messages[0].Body)
	require.NotNil(t, view.Messages[0].SenderName)
	require.Equal(t, bobName, *view.Messages[0].SenderName)
}
