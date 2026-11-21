//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo:
// typing indicators (PLAN-chat item 98) between real chatd processes over
// this fork's realtime server (rtSetTyping and the typing side of
// rtPollInbox). alice and bob share a team; dave is in none.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdV3Typing
//
// With KB_BIN set, `kb chat typing` and `kb chat listen --json` are checked
// too.

package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type typingEvent struct {
	Channel string `json:"channel"`
	Users   []struct {
		Username string `json:"username"`
		Party    string `json:"party"`
		UntilMs  int64  `json:"until_ms"`
	} `json:"users"`
	UntilMs int64 `json:"until_ms"`
}

// nextTyping waits for c's next typing event.
func nextTyping(t *testing.T, c *chatdClient, timeout time.Duration) typingEvent {
	t.Helper()
	var ev typingEvent
	f := c.event("typing", timeout)
	require.NoError(t, json.Unmarshal(f.Data, &ev), "%s", f.Data)
	require.NotNil(t, ev.Users, "a typing event always has a users list: %s", f.Data)
	return ev
}

// noTyping checks that no typing event reaches c for d. Other events are
// dropped.
func noTyping(t *testing.T, c *chatdClient, d time.Duration, why string) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case f := <-c.events:
			require.NotEqual(t, "typing", f.Event, "%s: got %s", why, f.Data)
		case <-deadline:
			return
		}
	}
}

func typingState(c *chatdClient, team map[string]any, on bool) string {
	var res struct {
		State string `json:"state"`
	}
	c.ok("typing", map[string]any{"team": team, "app": 1,
		"channel": map[string]any{"kind": "default"}, "on": on}, &res)
	return res.State
}

func TestChatdV3Typing(t *testing.T) {
	r := newM3Rig(t)
	defer r.close()
	defChan := map[string]any{"kind": "default"}

	teamNm := "Ty_" + r.rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	team := created.Team
	merklePoke(t)
	r.admitBob(t, team)
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": defChan, "body": "hello"}, nil)
	row := waitInbox(t, r.bob, "the team's channel", func(row m3InboxRow) bool {
		return strings.EqualFold(row.TeamName, teamNm)
	})
	aliceParty := ""
	for _, m := range threadMsgs(r.bob, team, defChan) {
		if m.Body == "hello" {
			aliceParty = m.Sender
		}
	}
	require.NotEmpty(t, aliceParty, "bob reads alice's first message")
	// Let both poll loops settle on their long-polls.
	waitInbox(t, r.alice, "the team's channel", func(row m3InboxRow) bool {
		return strings.EqualFold(row.TeamName, teamNm)
	})
	time.Sleep(time.Second)
	r.alice.drainEvents()
	r.bob.drainEvents()

	// alice types: bob's subscribed connection hears it, with her name.
	start := time.Now()
	require.Equal(t, "sent", typingState(r.alice, team, true))
	ev := nextTyping(t, r.bob, 10*time.Second)
	took := time.Since(start)
	t.Logf("typing reached bob after %v: %+v", took, ev)
	require.Less(t, took, 5*time.Second, "typing must arrive by wake, not by a poll's timeout")
	require.Equal(t, row.Channel.ID, ev.Channel)
	require.Len(t, ev.Users, 1)
	require.Equal(t, r.aliceNm, ev.Users[0].Username)
	require.Equal(t, aliceParty, ev.Users[0].Party, "the party is the one her messages carry")
	left := time.Until(time.UnixMilli(ev.Users[0].UntilMs))
	require.Greater(t, left, 3*time.Second)
	require.LessOrEqual(t, left, 10*time.Second, "a typing state holds for ten seconds at most")
	require.Equal(t, ev.Users[0].UntilMs, ev.UntilMs)
	firstUntil := time.UnixMilli(ev.UntilMs)

	// alice is not told about herself.
	noTyping(t, r.alice, 1500*time.Millisecond, "alice heard her own typing")

	// More key presses inside three seconds reach nobody.
	require.Equal(t, "throttled", typingState(r.alice, team, true))
	noTyping(t, r.bob, time.Second, "a throttled call was fanned out")

	// It expires by itself: nothing more is said, and the state bob was
	// given has run out. Typing again afterwards is a new state.
	if wait := time.Until(firstUntil) + 500*time.Millisecond; wait > 0 {
		noTyping(t, r.bob, wait, "an expiry needs no event")
	}
	require.Equal(t, "idle", typingState(r.alice, team, false), "nothing is left to stop")
	require.Equal(t, "sent", typingState(r.alice, team, true))
	ev = nextTyping(t, r.bob, 10*time.Second)
	require.Len(t, ev.Users, 1)
	require.True(t, time.UnixMilli(ev.UntilMs).After(firstUntil))

	// Stopping goes out at once: an empty list.
	require.Equal(t, "sent", typingState(r.alice, team, false))
	ev = nextTyping(t, r.bob, 10*time.Second)
	require.Equal(t, row.Channel.ID, ev.Channel)
	require.Empty(t, ev.Users)
	require.Zero(t, ev.UntilMs)

	// The setting "do not send typing": nothing leaves alice's device.
	r.alice.ok("settings_set", map[string]any{"set": map[string]any{"chat.typing_send": false}}, nil)
	time.Sleep(3 * time.Second) // past chatd's own throttle
	require.Equal(t, "off", typingState(r.alice, team, true))
	noTyping(t, r.bob, 2*time.Second, "typing was sent although the setting is off")
	// bob's is still on: alice hears him.
	r.alice.drainEvents()
	require.Equal(t, "sent", typingState(r.bob, team, true))
	ev = nextTyping(t, r.alice, 10*time.Second)
	require.Len(t, ev.Users, 1)
	require.Equal(t, r.bobNm, ev.Users[0].Username, "the name as bob typed it at signup")
	r.alice.ok("settings_set", map[string]any{"set": map[string]any{"chat.typing_send": true}}, nil)
	require.Equal(t, "sent", typingState(r.alice, team, true))
	ev = nextTyping(t, r.bob, 10*time.Second)
	require.Len(t, ev.Users, 1)

	// Messages arrive as before, and sending one ends alice's typing on
	// bob's side without waiting for it to run out.
	r.bob.drainEvents()
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": defChan, "body": "done typing"}, nil)
	sawStop, sawInbox := false, false
	deadline := time.After(20 * time.Second)
	for !sawStop || !sawInbox {
		select {
		case f := <-r.bob.events:
			switch f.Event {
			case "inbox_changed":
				sawInbox = true
			case "typing":
				var stop typingEvent
				require.NoError(t, json.Unmarshal(f.Data, &stop))
				require.Empty(t, stop.Users, "alice's send ends her typing")
				sawStop = true
			}
		case <-deadline:
			t.Fatalf("after alice's send: typing stopped=%v, inbox_changed=%v", sawStop, sawInbox)
		}
	}
	var bodies []string
	for _, m := range threadMsgs(r.bob, team, defChan) {
		bodies = append(bodies, m.Body)
	}
	require.Contains(t, bodies, "done typing")

	// dave is not in the team: he cannot type into it, and nobody hears.
	daveCfg := newTestAgent(t)
	dave := startChatdFresh(t, daveCfg.cfgFlags)
	dave.ok("subscribe", map[string]any{"app": 1}, nil)
	dave.signupOverWire(vHost(t, 4).Addr, "dave_"+r.rnd, "dave typing")
	dave.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	r.bob.drainEvents()
	r.alice.drainEvents()
	f := dave.call("typing", map[string]any{"team": team, "app": 1, "channel": defChan, "on": true})
	require.False(t, f.OK, "a non-member typed into the team: %s", f.Result)
	t.Logf("dave's typing was refused: %+v", f.Error)
	noTyping(t, r.bob, 1500*time.Millisecond, "a non-member's typing reached bob")
	noTyping(t, r.alice, 500*time.Millisecond, "a non-member's typing reached alice")

	if r.kbBin == "" {
		t.Log("KB_BIN is not set: the kb round is skipped")
		return
	}

	// kb chat listen --json on bob's chatd prints the event; kb chat typing
	// on alice's sends one.
	listen := exec.Command(r.kbBin, "--no-autostart", "chat", "listen", "--json", "--once")
	listen.Env = append(os.Environ(),
		"KB_CHATD_SOCKET=tcp://"+r.bob.addr,
		"KB_CHATD_TOKEN_FILE="+r.bob.tokenFile,
	)
	stdout, err := listen.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, listen.Start())
	defer func() {
		_ = listen.Process.Kill()
		_, _ = listen.Process.Wait()
	}()
	lines := make(chan map[string]any, 64)
	go func() {
		sc := bufio.NewScanner(stdout)
		for sc.Scan() {
			var line map[string]any
			if json.Unmarshal(sc.Bytes(), &line) == nil {
				lines <- line
			}
		}
		close(lines)
	}()
	waitLine := func(typ string) map[string]any {
		t.Helper()
		deadline := time.After(20 * time.Second)
		for {
			select {
			case line, ok := <-lines:
				require.True(t, ok, "kb chat listen ended before a %s line", typ)
				if line["type"] == typ {
					return line
				}
			case <-deadline:
				t.Fatalf("kb chat listen printed no %s line", typ)
			}
		}
	}
	waitLine("subscribed")
	time.Sleep(3 * time.Second) // past alice's last "typing"
	out := r.kb(t, r.alice, "chat", "typing", teamNm)
	require.Contains(t, out, "Told them you are typing")
	line := waitLine("typing")
	require.Equal(t, row.Channel.ID, line["channel"])
	users := line["users"].([]any)
	require.Len(t, users, 1)
	require.Equal(t, r.aliceNm, users[0].(map[string]any)["username"])
	out = r.kb(t, r.alice, "chat", "typing", teamNm, "--stop")
	require.Contains(t, out, "Told them you stopped typing")
	line = waitLine("typing")
	require.Empty(t, line["users"].([]any))
}
