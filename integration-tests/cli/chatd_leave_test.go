//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo:
// leaving a team over chatd's wire (PLAN-chat item 53, team_leave), and the
// key rotation an admin's chatd does afterwards.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdTeamLeave

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// keysDueSince is list_teams' rotation_due_since for team on c (0: not due,
// -1: not listed).
func keysDueSince(c *chatdClient, team string) int64 {
	var teams struct {
		Teams []struct {
			Team  map[string]string `json:"team"`
			Since int64             `json:"rotation_due_since"`
		} `json:"teams"`
	}
	c.ok("list_teams", nil, &teams)
	for _, tm := range teams.Teams {
		if strings.EqualFold(tm.Team["value"], team) {
			return tm.Since
		}
	}
	return -1
}

// waitKeysRotated polls c's list_teams until team's keys are no longer due.
func waitKeysRotated(t *testing.T, c *chatdClient, team string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if keysDueSince(c, team) == 0 {
			return
		}
		merklePoke(t)
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the keys of %s were not rotated within %v", team, within)
}

func joinAs(t *testing.T, admin, joiner *chatdClient, team map[string]any) {
	t.Helper()
	var inv struct {
		Code string `json:"code"`
	}
	admin.ok("create_team_invite", map[string]any{"team": team}, &inv)
	joiner.ok("accept_team_invite", map[string]any{"code": inv.Code}, nil)
	merklePoke(t)
	reqs := joinRequests(admin, team)
	require.Len(t, reqs, 1, "%+v", reqs)
	admin.ok("team_admit", map[string]any{"team": team, "requests": []map[string]any{{"rsvp": reqs[0].RSVP}}}, nil)
	merklePoke(t)
}

func TestChatdTeamLeave(t *testing.T) {
	r := newM3Rig(t)
	defer r.close()
	vh := vHost(t, 4)

	// carol joins as a third chatd. alice's chatd has a home of its own
	// here, so it can be stopped and started again on it.
	carolCfg := newTestAgent(t)
	carol := startChatdFresh(t, carolCfg.cfgFlags)
	carol.ok("subscribe", map[string]any{"app": 1}, nil)
	carolNm := "carol_" + r.rnd
	carol.signupOverWire(vh.Addr, carolNm, "carol leave")
	carol.event("user_changed", 10*time.Second)

	aliceCfg := newTestAgent(t)
	aliceDir := t.TempDir()
	r.alice.stop()
	alice := startChatdAt(t, aliceCfg.cfgFlags, aliceDir)
	alice.ok("subscribe", map[string]any{"app": 1}, nil)
	aliceNm := "alix_" + r.rnd
	alice.signupOverWire(vh.Addr, aliceNm, "alice leave")
	alice.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)

	teamNm := "Lv_" + r.rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	team := created.Team
	merklePoke(t)
	joinAs(t, alice, r.bob, team)
	joinAs(t, alice, carol, team)
	require.True(t, memberOf(r.bob, teamNm))
	require.True(t, memberOf(carol, teamNm))
	sendText(alice, team, "before")
	merklePoke(t)
	require.Equal(t, int64(0), keysDueSince(carol, teamNm))

	// The admin is offline when bob leaves.
	alice.stop()

	// The last owner can't leave (checked on bob's side first: a member
	// who is not the last owner can).
	r.bob.drainEvents()
	if r.kbBin != "" {
		// Without --yes, kb asks for the team's name.
		out, errOut, code := kbRunEnv(t, r.kbBin, r.bob, nil, "wrong\n", "team", "leave", teamNm)
		require.NotEqual(t, 0, code, "%s %s", out, errOut)
		require.Contains(t, errOut, "still in it")
		require.True(t, memberOf(r.bob, teamNm))
		out = r.kb(t, r.bob, "team", "leave", teamNm, "--yes")
		require.Contains(t, out, "You left "+teamNm)
	} else {
		r.bob.ok("team_leave", map[string]any{"team": team}, nil)
	}
	ev := r.bob.event("teams_changed", 10*time.Second)
	require.Contains(t, string(ev.Data), `"left"`)
	merklePoke(t)

	// bob: the team is gone, and he can neither post nor read.
	require.False(t, memberOf(r.bob, teamNm))
	for _, row := range inboxRows(r.bob) {
		require.NotEqual(t, strings.ToLower(teamNm), strings.ToLower(row.TeamName), "bob's inbox still has the team")
	}
	f := r.bob.call("send", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "body": "still here?"})
	require.False(t, f.OK)
	f = r.bob.call("get_thread_view", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "before": 0, "num": 50})
	require.False(t, f.OK)
	// Leaving twice is a clear refusal.
	f = r.bob.call("team_leave", map[string]any{"team": team})
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "not a member of "+teamNm)

	// carol sees "bob left the team", bob gone from the members, and the
	// keys due (she is a member: not hers to rotate).
	msgs := threadMsgs(carol, team, defaultChannel())
	var left *m3Msg
	for i := range msgs {
		if msgs[i].Body == `{"kbf":1,"sys":{"type":"left"}}` {
			left = &msgs[i]
		}
	}
	require.NotNil(t, left, "no left line in %+v", msgs)
	require.NotNil(t, left.SenderName)
	require.True(t, strings.EqualFold(*left.SenderName, r.bobNm), "left line from %v", *left.SenderName)
	_, still := teamRoles(carol, team)[strings.ToLower(r.bobNm)]
	require.False(t, still)
	since := keysDueSince(carol, teamNm)
	require.Greater(t, since, int64(0), "keys due after bob left")
	// A plain member can't take anyone out either.
	f = carol.call("team_set_role", map[string]any{"team": team, "user": aliceNm, "role": map[string]any{"kind": "none"}})
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)

	// alice's chatd comes back and rotates on its own.
	alice = startChatdAt(t, aliceCfg.cfgFlags, aliceDir)
	alice.ok("subscribe", map[string]any{"app": 1}, nil)
	start := time.Now()
	ev = alice.event("teams_changed", 90*time.Second)
	require.Contains(t, string(ev.Data), `"keys_rotated"`)
	t.Logf("alice's chatd rotated %v after it started", time.Since(start).Round(100*time.Millisecond))
	merklePoke(t)
	merklePoke(t)
	waitKeysRotated(t, carol, teamNm, 60*time.Second)
	var diag struct {
		TeamKeysDue []any `json:"team_keys_due"`
	}
	alice.ok("diagnostics", nil, &diag)
	require.Empty(t, diag.TeamKeysDue)

	// Written after the rotation: carol reads it, bob's device can't.
	sendText(alice, team, "after")
	merklePoke(t)
	require.Contains(t, bodies(threadMsgs(carol, team, defaultChannel())), "after")
	f = r.bob.call("get_thread_view", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "before": 0, "num": 50})
	require.False(t, f.OK)

	// alice is the last owner.
	f = alice.call("team_leave", map[string]any{"team": team})
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "last owner")
	require.True(t, memberOf(alice, teamNm))

	// An owner who is not the last can leave; carol's chatd, an owner's,
	// is online and rotates.
	alice.ok("team_set_role", map[string]any{"team": team, "user": carolNm, "role": map[string]any{"kind": "owner"}}, nil)
	merklePoke(t)
	carol.drainEvents()
	alice.ok("team_leave", map[string]any{"team": team}, nil)
	merklePoke(t)
	require.False(t, memberOf(alice, teamNm))
	require.Equal(t, "owner", teamRoles(carol, team)[strings.ToLower(carolNm)])
	start = time.Now()
	waitKeysRotated(t, carol, teamNm, 120*time.Second)
	t.Logf("carol's chatd rotated %v after alice left", time.Since(start).Round(100*time.Millisecond))

	// carol is now the last owner.
	f = carol.call("team_leave", map[string]any{"team": team})
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)

	if r.kbBin != "" {
		out := r.kb(t, carol, "team", "api", "-m", `{"method": "leave-team", "params": {"options": {"team": "`+teamNm+`"}}}`)
		var res map[string]json.RawMessage
		require.NoError(t, json.Unmarshal([]byte(strings.TrimSpace(out)), &res), out)
		require.Contains(t, string(res["error"]), "last owner")
	}
}
