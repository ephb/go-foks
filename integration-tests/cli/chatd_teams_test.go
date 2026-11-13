//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo:
// team management over chatd's wire (PLAN-chat items 51-53): declining a
// join request, changing a member's role and removing a member.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdTeams

package cli

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func teamRoles(c *chatdClient, team map[string]any) map[string]string {
	var members struct {
		Members []struct {
			Name string         `json:"name"`
			Role map[string]any `json:"role"`
		} `json:"members"`
	}
	c.ok("team_members", map[string]any{"team": team}, &members)
	roles := map[string]string{}
	for _, m := range members.Members {
		roles[strings.ToLower(m.Name)] = m.Role["kind"].(string)
	}
	return roles
}

// requestJoin has bob accept a fresh invite to team and returns the request.
func (r *m3Rig) requestJoin(t *testing.T, team map[string]any) m3JoinRequest {
	t.Helper()
	var inv struct {
		Code string `json:"code"`
	}
	r.alice.ok("create_team_invite", map[string]any{"team": team}, &inv)
	r.bob.ok("accept_team_invite", map[string]any{"code": inv.Code}, nil)
	merklePoke(t)
	reqs := joinRequests(r.alice, team)
	require.Len(t, reqs, 1, "%+v", reqs)
	return reqs[0]
}

func (r *m3Rig) admitBob(t *testing.T, team map[string]any) {
	t.Helper()
	req := r.requestJoin(t, team)
	r.alice.ok("team_admit", map[string]any{"team": team, "requests": []map[string]any{{"rsvp": req.RSVP}}}, nil)
	merklePoke(t)
}

func TestChatdTeams(t *testing.T) {
	r := newM3Rig(t)
	defer r.close()
	bob := strings.ToLower(r.bobNm)
	alice := strings.ToLower(r.aliceNm)

	teamNm := "Tm_" + r.rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	team := created.Team
	merklePoke(t)

	// 51: decline a request. bob is not a member and the list is empty.
	req := r.requestJoin(t, team)
	f := r.bob.call("team_reject", map[string]any{"team": team, "rsvps": []string{req.RSVP}})
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "Only an admin or owner of this team can decline join requests")
	r.alice.ok("team_reject", map[string]any{"team": team, "rsvps": []string{req.RSVP}}, nil)
	merklePoke(t)
	require.Empty(t, joinRequests(r.alice, team), "declined requests leave the list")
	require.False(t, memberOf(r.bob, teamNm))
	// Declining twice is fine (FOKS does not check the state).
	r.alice.ok("team_reject", map[string]any{"team": team, "rsvps": []string{req.RSVP}}, nil)
	f = r.alice.call("team_reject", map[string]any{"team": team, "rsvps": []string{}})
	require.Equal(t, "invalid_input", f.Error.Code)

	// bob asks again and is admitted.
	r.admitBob(t, team)
	require.True(t, memberOf(r.bob, teamNm))
	require.Equal(t, "member", teamRoles(r.alice, team)[bob])

	// 52: a member can't change roles (FOKS's nil admin key used to crash
	// chatd here); an admin can.
	f = r.bob.call("team_set_role", map[string]any{"team": team, "user": r.aliceNm, "role": map[string]any{"kind": "member", "viz_level": 0}})
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)
	require.Equal(t, "Only an admin or owner of this team can change roles.", f.Error.Message)
	r.alice.ok("team_set_role", map[string]any{"team": team, "user": r.bobNm, "role": map[string]any{"kind": "admin"}}, nil)
	merklePoke(t)
	require.Equal(t, "admin", teamRoles(r.alice, team)[bob])
	f = r.alice.call("team_set_role", map[string]any{"team": team, "user": "nobody_" + r.rnd, "role": map[string]any{"kind": "admin"}})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "isn't a member of this team")
	// Nobody changes their own role (FOKS only takes team-key self changes).
	f = r.bob.call("team_set_role", map[string]any{"team": team, "user": r.bobNm, "role": map[string]any{"kind": "none"}})
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	f = r.alice.call("team_set_role", map[string]any{"team": team, "user": r.aliceNm, "role": map[string]any{"kind": "admin"}})
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	require.Equal(t, "owner", teamRoles(r.alice, team)[alice])
	// bob, an admin now, sets alice's role: owner -> admin needs an owner.
	f = r.bob.call("team_set_role", map[string]any{"team": team, "user": r.aliceNm, "role": map[string]any{"kind": "admin"}})
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)
	require.Equal(t, "Only an owner of this team can make someone an owner or change an owner's role.", f.Error.Message)

	// 53: alice removes bob (back to member first, then none).
	r.alice.ok("team_set_role", map[string]any{"team": team, "user": r.bobNm, "role": map[string]any{"kind": "member", "viz_level": 0}}, nil)
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "before"}, nil)
	merklePoke(t)
	r.alice.ok("team_set_role", map[string]any{"team": team, "user": r.bobNm, "role": map[string]any{"kind": "none"}}, nil)
	merklePoke(t)
	_, still := teamRoles(r.alice, team)[bob]
	require.False(t, still, "bob was removed")
	// The team leaves bob's list, and he can neither post nor read.
	require.False(t, memberOf(r.bob, teamNm))
	var rows []string
	for _, row := range inboxRows(r.bob) {
		rows = append(rows, row.TeamName+"#"+row.Channel.Name)
	}
	t.Logf("bob's inbox after removal: %v", rows)
	f = r.bob.call("send", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "still here?"})
	require.False(t, f.OK)
	f = r.bob.call("get_thread_view", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "before": 0, "num": 50})
	require.False(t, f.OK)

	// 59: user_keys. bob sees the fingerprint alice sees for herself; a
	// new key (a backup key) changes it.
	type userKeys struct {
		User        string `json:"user"`
		Fingerprint string `json:"fingerprint"`
		Devices     []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"devices"`
	}
	var mine, seen userKeys
	r.alice.ok("user_keys", map[string]any{"user": r.aliceNm}, &mine)
	r.bob.ok("user_keys", map[string]any{"user": r.aliceNm}, &seen)
	t.Logf("alice's keys: %+v", seen)
	require.Len(t, seen.Devices, 1)
	require.Equal(t, mine.Fingerprint, seen.Fingerprint)
	require.Len(t, strings.Fields(seen.Fingerprint), 8)
	r.alice.ok("create_backup_key", nil, nil)
	merklePoke(t)
	merklePoke(t)
	var later userKeys
	r.bob.ok("user_keys", map[string]any{"user": r.aliceNm}, &later)
	require.Len(t, later.Devices, 2)
	require.NotEqual(t, seen.Fingerprint, later.Fingerprint, "a new key changes the fingerprint")
	f = r.bob.call("user_keys", map[string]any{"user": "nobody_" + r.rnd})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "Couldn't find nobody_")

	if r.kbBin == "" {
		return
	}
	// The same through kb: reject, role, remove.
	r.requestJoin(t, team)
	out := r.kb(t, r.alice, "team", "reject", teamNm, r.bobNm)
	require.Contains(t, out, "Declined "+r.bobNm)
	merklePoke(t)
	require.Empty(t, joinRequests(r.alice, team))
	r.admitBob(t, team)
	out = r.kb(t, r.alice, "team", "role", teamNm, r.bobNm, "admin")
	require.Contains(t, out, r.bobNm+" is now admin in "+teamNm)
	merklePoke(t)
	require.Equal(t, "admin", teamRoles(r.alice, team)[bob])
	// kb team api (PLAN-chat 62): keybase's shape over the same ops.
	out = kbRun(t, r.kbBin, r.alice, `{"method": "list-team-memberships", "params": {"options": {"team": "`+teamNm+`"}}}`, "team", "api")
	t.Logf("kb team api: %s", out)
	require.Contains(t, out, `"admins":[{"status":0,"username":"`+r.bobNm+`"}]`)
	out = r.kb(t, r.alice, "team", "api", "-m", `{"method": "edit-member", "params": {"options": {"team": "`+teamNm+`", "username": "`+r.bobNm+`", "role": "writer"}}}`)
	require.Contains(t, out, `{"result":{}}`)
	merklePoke(t)
	require.Equal(t, "member", teamRoles(r.alice, team)[bob])
	out = r.kb(t, r.alice, "team", "remove", teamNm, r.bobNm)
	require.Contains(t, out, "Removed "+r.bobNm+" from "+teamNm)
	merklePoke(t)
	_, still = teamRoles(r.alice, team)[bob]
	require.False(t, still)
}
