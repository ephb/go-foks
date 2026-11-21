//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo: a
// conversation's muted and hidden flags on the server (PLAN-chat item 97,
// rtSetChannelFlags) between real chatd processes. alice has two devices
// (two chatds) and shares a team with bob.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdV3ChannelFlags
//
// With KB_BIN set, `kb chat mute|unmute|hide` and `kb chat ls` are checked
// too.

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type flagsRow struct {
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	TeamName string `json:"team_name"`
	Muted    *bool  `json:"muted"`
	Hidden   *bool  `json:"hidden"`
}

// waitFlags polls c's inbox until the team's row has these flags.
func waitFlags(t *testing.T, c *chatdClient, team string, muted, hidden bool, what string) flagsRow {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last flagsRow
	for time.Now().Before(deadline) {
		var v struct {
			Rows []flagsRow `json:"rows"`
		}
		c.ok("inbox_view", map[string]any{"app": 1, "local_only": false}, &v)
		for _, row := range v.Rows {
			if !strings.EqualFold(row.TeamName, team) {
				continue
			}
			last = row
			require.NotNil(t, row.Muted, "inbox_view rows carry muted")
			require.NotNil(t, row.Hidden, "inbox_view rows carry hidden")
			if *row.Muted == muted && *row.Hidden == hidden {
				return row
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("%s: the row never became muted=%v hidden=%v; last %+v", what, muted, hidden, last)
	return flagsRow{}
}

func setFlags(c *chatdClient, team map[string]any, flags map[string]any) string {
	p := map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}}
	for k, v := range flags {
		p[k] = v
	}
	var res struct {
		State string `json:"state"`
	}
	c.ok("set_channel_flags", p, &res)
	return res.State
}

func TestChatdV3ChannelFlags(t *testing.T) {
	r := newM3Rig(t)
	defer r.close()
	defChan := map[string]any{"kind": "default"}
	vh := vHost(t, 4)

	teamNm := "Fl_" + r.rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	team := created.Team
	merklePoke(t)
	r.admitBob(t, team)
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": defChan, "body": "hello"}, nil)
	waitFlags(t, r.bob, teamNm, false, false, "bob's row")
	desk := r.alice

	// alice's second device.
	lapCfg := newTestAgent(t)
	laptop := startChatdFresh(t, lapCfg.cfgFlags)
	laptop.ok("subscribe", map[string]any{"app": 1}, nil)
	var onDesk, onLap kexStart
	desk.ok("device_assist_start", nil, &onDesk)
	laptop.ok("device_login_start", map[string]any{"server": string(vh.Addr), "username": r.aliceNm, "device": "laptop"}, &onLap)
	desk.ok("device_assist_answer", map[string]any{"session": onDesk.Session, "code": onLap.Code}, nil)
	d := desk.kexDone("device_assist_done")
	require.True(t, d.OK, "%+v", d.Error)
	d = laptop.kexDone("device_login_done")
	require.True(t, d.OK, "%+v", d.Error)
	laptop.event("user_changed", 30*time.Second)
	merklePoke(t)
	merklePoke(t)
	row := waitFlags(t, laptop, teamNm, false, false, "the laptop's row")
	chid := row.Channel.ID

	// Muted on the desk: the laptop shows it, bob's row is his own.
	require.Equal(t, "set", setFlags(desk, team, map[string]any{"muted": true}))
	start := time.Now()
	waitFlags(t, laptop, teamNm, true, false, "muted on the desk")
	t.Logf("the laptop saw the mute after %v", time.Since(start))
	waitFlags(t, desk, teamNm, true, false, "the desk's own row")
	waitFlags(t, r.bob, teamNm, false, false, "bob's row after alice muted")

	// Neither flag is not a request.
	f := desk.call("set_channel_flags", map[string]any{"team": team, "app": 1, "channel": defChan})
	require.False(t, f.OK)
	require.Equal(t, "invalid_input", f.Error.Code)

	// Hidden from the laptop, and back with bob's next message; muted
	// stays through both.
	require.Equal(t, "set", setFlags(laptop, team, map[string]any{"hidden": true}))
	waitFlags(t, desk, teamNm, true, true, "hidden on the laptop")
	r.bob.ok("send", map[string]any{"team": team, "app": 1, "channel": defChan, "body": "are you there?"}, nil)
	waitFlags(t, desk, teamNm, true, false, "bob's message brings it back")
	waitFlags(t, laptop, teamNm, true, false, "bob's message brings it back on the laptop")
	require.Equal(t, "set", setFlags(laptop, team, map[string]any{"muted": false}))
	waitFlags(t, desk, teamNm, false, false, "unmuted on the laptop")

	// A client that only writes the synced settings, as kb-gui does:
	// chatd carries mute and hide over to the server's flags.
	desk.ok("settings_set", map[string]any{"set": map[string]any{
		"chat.conv." + chid + ".mute": true,
		"chat.conv." + chid + ".hide": 2,
	}}, nil)
	waitFlags(t, laptop, teamNm, true, true, "mute and hide settings written on the desk")
	desk.ok("settings_set", map[string]any{"remove": []string{
		"chat.conv." + chid + ".mute", "chat.conv." + chid + ".hide",
	}}, nil)
	waitFlags(t, laptop, teamNm, false, false, "the settings removed on the desk")

	// Someone who is not in the team has no flags to set there.
	carolChatd := startChatdFresh(t, newTestAgent(t).cfgFlags)
	carolChatd.ok("subscribe", map[string]any{"app": 1}, nil)
	carolChatd.signupOverWire(vh.Addr, "carol_"+r.rnd, "carol flags")
	carolChatd.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	f = carolChatd.call("set_channel_flags", map[string]any{"team": team, "app": 1, "channel": defChan, "muted": true})
	require.False(t, f.OK, "a non-member set a flag")
	t.Logf("a non-member's set_channel_flags: %+v", f.Error)

	if r.kbBin == "" {
		t.Log("KB_BIN is not set: kb chat mute/hide not checked")
		return
	}
	teamArg := strings.ToLower(teamNm)

	// kb on the desk mutes; the laptop's inbox and its `kb chat ls` show it.
	out := r.kb(t, desk, "chat", "mute", teamArg)
	t.Logf("kb chat mute: %s", strings.TrimSpace(out))
	require.Contains(t, out, "Muted")
	waitFlags(t, laptop, teamNm, true, false, "kb chat mute on the desk")
	ls := r.kb(t, laptop, "chat", "ls")
	t.Logf("kb chat ls on the laptop:\n%s", ls)
	require.Contains(t, ls, "[muted]")

	// kb on the laptop unmutes and hides.
	r.kb(t, laptop, "chat", "unmute", teamArg)
	waitFlags(t, desk, teamNm, false, false, "kb chat unmute on the laptop")
	out = r.kb(t, laptop, "chat", "hide", teamArg)
	require.Contains(t, out, "Hid")
	waitFlags(t, desk, teamNm, false, true, "kb chat hide on the laptop")
	ls = r.kb(t, desk, "chat", "ls")
	require.NotContains(t, strings.ToLower(ls), teamArg, "a hidden conversation is listed:\n%s", ls)
	ls = r.kb(t, desk, "chat", "ls", "--all")
	require.Contains(t, ls, "[hidden]", "kb chat ls --all:\n%s", ls)
	r.kb(t, desk, "chat", "unhide", teamArg)
	waitFlags(t, laptop, teamNm, false, false, "kb chat unhide on the desk")
}
