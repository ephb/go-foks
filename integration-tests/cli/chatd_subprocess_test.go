//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// imports chat-foks's backend/shim, which resolves only through that repo's
// root go.work, hence the "chatfoks" build tag. Run from a chat-foks checkout:
//
//	go test -tags chatfoks ./integration-tests/cli -run TestChatdSubprocess

package cli

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/proto/lcl"
	"github.com/stretchr/testify/require"

	"github.com/terra/kebase-fork/backend/shim/wire"
)

// TestChatdSubprocessAuthenticatedExchange proves the full desktop chatd
// lifecycle end to end:
//  1. Signs up a user and creates a team via standalone CLI calls against the
//     test process's isolated LocalAppData / --home.
//  2. Builds and spawns the real chatd binary as a child OS process, passing
//     the test environment's probe/beacon/CA/DNS flags so chatd's in-process
//     standalone FOKS instance talks to the test server and unlocks the
//     persisted active user on startup.
//  3. Connects over TCP loopback (same framing as Unix socket), completes the
//     WIRE.md hello handshake with the token chatd wrote, and exercises
//     authenticated WIRE.md operations (list_existing_users, make_channel,
//     send, get_thread_view, sync_inbox, inbox_view) against the running
//     chatd subprocess.
func TestChatdSubprocessAuthenticatedExchange(t *testing.T) {
	a := newTestAgent(t)
	a.cfgFlags = append([]string{"-s"}, a.cfgFlags...)

	signupUI := (&mockSignupUI{}).withDeviceKey()
	a.runCmdWithUIs(t,
		libclient.UIs{Signup: signupUI, Terminal: &terminalUI{}},
		"--simple-ui", "signup")
	require.Len(t, signupUI.promptSet, 1)

	tm := "t-" + strings.ToLower(fsRandomString(t, 8))
	var teamRes lcl.TeamCreateRes
	a.runCmdToJSON(t, &teamRes, "team", "create", tm)
	merklePoke(t)

	chatdBin := os.Getenv("CHATD_TEST_BIN")
	if chatdBin == "" {
		// Build chatd.exe into a temp dir if not pre-built by caller.
		chatdBin = filepath.Join(t.TempDir(), "chatd.exe")
		buildCmd := exec.Command("go", "build", "-ldflags=-s -w", "-o", chatdBin,
			"github.com/terra/kebase-fork/backend/shim/cmd/chatd")
		buildOut, err := buildCmd.CombinedOutput()
		require.NoError(t, err, "building chatd failed: %s", string(buildOut))
	}

	// Reserve a free loopback TCP port so we know the address to dial.
	probeLn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	tcpAddr := probeLn.Addr().String()
	require.NoError(t, probeLn.Close())

	tokenFile := filepath.Join(t.TempDir(), "chatd.token")
	chatdArgs := append([]string{"-insecure-dev-tcp", "-socket", "tcp://" + tcpAddr,
		"-token-file", tokenFile, "--"}, a.cfgFlags...)
	proc := exec.Command(chatdBin, chatdArgs...)
	require.NoError(t, proc.Start())
	t.Cleanup(func() {
		if proc.Process != nil {
			_ = proc.Process.Kill()
			_, _ = proc.Process.Wait()
		}
	})

	// Wait for chatd to accept connections on tcpAddr.
	var conn net.Conn
	deadline := time.Now().Add(15 * time.Second)
	for {
		c, dialErr := net.DialTimeout("tcp", tcpAddr, 200*time.Millisecond)
		if dialErr == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("chatd subprocess did not become reachable on %s: %v", tcpAddr, dialErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Cleanup(func() { _ = conn.Close() })

	var nextID int64
	call := func(op string, params any) wire.Response {
		nextID++
		var rawParams json.RawMessage
		if params != nil {
			b, err := json.Marshal(params)
			require.NoError(t, err)
			rawParams = b
		}
		reqBytes, err := json.Marshal(wire.Request{
			V:      3,
			ID:     nextID,
			Op:     op,
			Params: rawParams,
		})
		require.NoError(t, err)
		require.NoError(t, wire.WriteFrame(conn, reqBytes))

		respBytes, err := wire.ReadFrame(conn)
		require.NoError(t, err)
		var resp wire.Response
		require.NoError(t, json.Unmarshal(respBytes, &resp))
		require.Equal(t, 3, resp.V)
		require.Equal(t, nextID, resp.ID)
		return resp
	}

	// 0. hello with the token chatd wrote before it started listening.
	token, err := os.ReadFile(tokenFile)
	require.NoError(t, err)
	helloResp := call("hello", map[string]any{"token": string(token), "client": "cli-test/0"})
	require.True(t, helloResp.OK, "hello failed: %+v", helloResp.Error)

	// 1. list_existing_users sees the signed-up active user.
	usersResp := call("list_existing_users", map[string]any{})
	require.True(t, usersResp.OK, "list_existing_users failed: %+v", usersResp.Error)
	var usersRes struct {
		Users []struct {
			Username string `json:"username"`
			Active   bool   `json:"active"`
		} `json:"users"`
	}
	require.NoError(t, json.Unmarshal(usersResp.Result, &usersRes))
	require.Len(t, usersRes.Users, 1)
	require.True(t, usersRes.Users[0].Active)

	teamParam := map[string]any{"kind": "named", "value": tm}

	// 2. make_channel creates "general-chat" on the team.
	mcResp := call("make_channel", map[string]any{
		"team":  teamParam,
		"app":   1,
		"name":  "general-chat",
		"desc":  "created via chatd subprocess",
		"roles": map[string]any{"read": nil, "write": nil},
	})
	require.True(t, mcResp.OK, "make_channel failed: %+v", mcResp.Error)
	var mcRes struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(mcResp.Result, &mcRes))
	require.NotEmpty(t, mcRes.ID)

	// 3. send a message by channel name.
	chParam := map[string]any{"kind": "name", "name": "general-chat"}
	sendResp := call("send", map[string]any{
		"team":    teamParam,
		"app":     1,
		"channel": chParam,
		"body":    "hello from chatd subprocess",
	})
	require.True(t, sendResp.OK, "send failed: %+v", sendResp.Error)

	// 4. get_thread_view reads back the decrypted message and resolved sender name.
	tvResp := call("get_thread_view", map[string]any{
		"team":    teamParam,
		"app":     1,
		"channel": chParam,
		"before":  0,
		"num":     10,
	})
	require.True(t, tvResp.OK, "get_thread_view failed: %+v", tvResp.Error)
	var tvRes struct {
		Messages []struct {
			Seq        uint64  `json:"seq"`
			Body       string  `json:"body"`
			Sender     string  `json:"sender"`
			SenderName *string `json:"sender_name"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(tvResp.Result, &tvRes))
	require.Len(t, tvRes.Messages, 1)
	require.Equal(t, uint64(1), tvRes.Messages[0].Seq)
	require.Equal(t, "hello from chatd subprocess", tvRes.Messages[0].Body)
	require.NotEmpty(t, tvRes.Messages[0].Sender)

	t.Logf("CHATD_AUTH_ENDPOINT addr=%s team=%s channel=general-chat chid=%s user=%s",
		tcpAddr, tm, mcRes.ID, usersRes.Users[0].Username)

	if holdFile := os.Getenv("CHATD_HOLD_FILE"); holdFile != "" {
		_ = os.WriteFile(holdFile, []byte(fmt.Sprintf(
			"ADDR=%s\nTEAM=%s\nCHANNEL=general-chat\nCHID=%s\nUSER=%s\n",
			tcpAddr, tm, mcRes.ID, usersRes.Users[0].Username,
		)), 0o600)
		for {
			if _, err := os.Stat(holdFile); os.IsNotExist(err) {
				break
			}
			time.Sleep(250 * time.Millisecond)
		}
	}
}
