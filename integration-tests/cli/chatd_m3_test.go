//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives two real chatd processes (alice and bob, each with its own FOKS
// home) through the roadmap M3 additions to WIRE.md version 3. Run from a
// chat-foks checkout (a prebuilt chatd saves ~2 minutes):
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdM3
//
// With KB_BIN set, the same scenario is also checked through the kb CLI.

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// m3Rig is two chatds on one open-viewership vhost, both signed up over
// the wire, plus a CLI agent (carol) that set the vhost up.
type m3Rig struct {
	t        *testing.T
	alice    *chatdClient
	bob      *chatdClient
	aliceNm  string
	bobNm    string // mixed case, as typed at signup
	rnd      string
	kbBin    string
	carol    *testAgent
	carolNm  string
	stopPoke func()
}

func newM3Rig(t *testing.T) *m3Rig {
	stopper := runMerkleActivePoker(t)
	vh := vHost(t, 4)

	carol := newTestAgent(t)
	carol.runAgent(t)
	newUserWithAgentAtVHost(t, carol, 4)
	merklePoke(t)
	merklePoke(t)
	setVHostOpenViewership(t, carol)
	carolNm := string(getActiveUser(t, carol).Info.Username.NameUtf8)

	rnd := strings.ToLower(fsRandomString(t, 6))
	r := &m3Rig{t: t, rnd: rnd, carol: carol, carolNm: carolNm, stopPoke: stopper, kbBin: os.Getenv("KB_BIN")}
	t.Cleanup(func() { carol.stop(t) })

	aliceCfg := newTestAgent(t)
	r.alice = startChatdFresh(t, aliceCfg.cfgFlags)
	r.alice.ok("subscribe", map[string]any{"app": 1}, nil)
	r.aliceNm = "alice_" + rnd
	r.alice.signupOverWire(vh.Addr, r.aliceNm, "alice m3")
	r.alice.event("user_changed", 10*time.Second)

	bobCfg := newTestAgent(t)
	r.bob = startChatdFresh(t, bobCfg.cfgFlags)
	r.bob.ok("subscribe", map[string]any{"app": 1}, nil)
	// Typed with capitals: FOKS keeps this form (NameUtf8) and matches on
	// the normalized one ("bob_m3xyz...").
	r.bobNm = "Bob_M3" + strings.ToUpper(rnd[:2]) + rnd[2:]
	r.bob.signupOverWire(vh.Addr, r.bobNm, "bob m3")
	r.bob.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	return r
}

func (r *m3Rig) close() { r.stopPoke() }

// kb runs the kb CLI against chatd c (KB_BIN must be set).
func (r *m3Rig) kb(t *testing.T, c *chatdClient, args ...string) string {
	t.Helper()
	return kbRun(t, r.kbBin, c, "", args...)
}

type m3InboxRow struct {
	Channel struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Archived bool   `json:"archived"`
	} `json:"channel"`
	TeamName    string  `json:"team_name"`
	TeamDisplay *string `json:"team_display"`
	TeamKind    string  `json:"team_kind"`
	Snippet     *string `json:"snippet"`
	LastSender  string  `json:"last_sender"`
	Unread      int     `json:"unread"`
}

func inboxRows(c *chatdClient) []m3InboxRow {
	var v struct {
		Rows []m3InboxRow `json:"rows"`
	}
	c.ok("inbox_view", map[string]any{"app": 1, "local_only": false}, &v)
	return v.Rows
}

// waitInbox polls inbox_view until pred finds a row, and returns it.
func waitInbox(t *testing.T, c *chatdClient, what string, pred func(m3InboxRow) bool) m3InboxRow {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var last []m3InboxRow
	for time.Now().Before(deadline) {
		last = inboxRows(c)
		for _, row := range last {
			if pred(row) {
				return row
			}
		}
		merklePoke(t)
		time.Sleep(300 * time.Millisecond)
	}
	b, _ := json.Marshal(last)
	t.Fatalf("inbox never showed %s; last rows: %s", what, b)
	return m3InboxRow{}
}

type m3Msg struct {
	MsgID      string  `json:"msg_id"`
	Seq        int     `json:"seq"`
	Typ        string  `json:"typ"`
	Sender     string  `json:"sender"`
	SenderName *string `json:"sender_name"`
	Body       string  `json:"body"`
	ReplyTo    *string `json:"reply_to"`
}

func threadMsgs(c *chatdClient, team, channel map[string]any) []m3Msg {
	var v struct {
		Messages []m3Msg `json:"messages"`
	}
	c.ok("get_thread_view", map[string]any{"team": team, "app": 1, "channel": channel, "before": 0, "num": 50}, &v)
	return v.Messages
}

func TestChatdM3(t *testing.T) {
	r := newM3Rig(t)
	defer r.close()

	t.Run("DisplayNames", r.displayNames)
	t.Run("RepliesReactionsEditsDeletes", r.typedMessages)
	t.Run("Channels", r.channels)
	t.Run("InvitesAndJoins", r.invitesAndJoins)
}

// displayNames: a DM with bob is listed with bob's name as he typed it
// (team_display), not FOKS's normalized form (team_name), once alice's
// chatd learned it -- from a thread's sender names or, before bob ever
// wrote, from the ad-hoc team's roster in the background.
func (r *m3Rig) displayNames(t *testing.T) {
	var dm struct {
		Team    map[string]any `json:"team"`
		Channel map[string]any `json:"channel"`
	}
	// alice names bob as he typed it; FOKS normalizes.
	r.alice.ok("start_dm", map[string]any{"users": []string{r.bobNm}}, &dm)
	r.alice.ok("send", map[string]any{"team": dm.Team, "app": 1, "channel": dm.Channel, "body": "hi Bob"}, nil)
	merklePoke(t)

	row := waitInbox(t, r.alice, "the DM with bob's display name", func(row m3InboxRow) bool {
		return row.TeamKind == "adhoc" && row.TeamDisplay != nil && *row.TeamDisplay == r.bobNm
	})
	t.Logf("DM row: team_name=%q team_display=%q", row.TeamName, *row.TeamDisplay)
	require.Equal(t, strings.ToLower(r.bobNm), row.TeamName, "team_name stays FOKS's normalized form")

	// bob sees alice (all lower case) and his own name is not in the row.
	bobRow := waitInbox(t, r.bob, "the DM on bob's side", func(row m3InboxRow) bool {
		return row.TeamKind == "adhoc" && row.TeamDisplay != nil && *row.TeamDisplay == r.aliceNm
	})
	require.Equal(t, r.aliceNm, bobRow.TeamName)

	// The team_display form works as a Team value too (FOKS normalizes).
	r.bob.ok("send", map[string]any{"team": map[string]any{"kind": "adhoc", "value": r.aliceNm + "," + r.bobNm},
		"app": 1, "channel": map[string]any{"kind": "default"}, "body": "hi Alice"}, nil)
	merklePoke(t)

	msgs := threadMsgs(r.alice, dm.Team, dm.Channel)
	require.Len(t, msgs, 2)
	require.NotNil(t, msgs[0].SenderName)
	require.Equal(t, r.bobNm, *msgs[0].SenderName, "sender names are display names")

	// Named teams: as created.
	teamNm := "Team_M3" + strings.ToUpper(r.rnd[:1]) + r.rnd[1:]
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	merklePoke(t)
	var teams struct {
		Teams []struct {
			Team map[string]string `json:"team"`
		} `json:"teams"`
	}
	r.alice.ok("list_teams", nil, &teams)
	t.Logf("create_team -> %v; list_teams -> %+v", created.Team, teams.Teams)
	r.alice.ok("send", map[string]any{"team": created.Team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "team hello"}, nil)
	merklePoke(t)
	teamRow := waitInbox(t, r.alice, "the new team", func(row m3InboxRow) bool {
		return row.TeamKind == "named" && strings.EqualFold(row.TeamName, teamNm)
	})
	t.Logf("team row: team_name=%q team_display=%v", teamRow.TeamName, teamRow.TeamDisplay)
	require.NotNil(t, teamRow.TeamDisplay)
	require.Equal(t, teamNm, *teamRow.TeamDisplay, "a named team is shown as created")

	if r.kbBin == "" {
		t.Log("KB_BIN not set: skipping the kb checks")
		return
	}
	out := r.kb(t, r.alice, "chat", "ls")
	t.Logf("kb chat ls:\n%s", out)
	require.Contains(t, out, "dm   "+r.aliceNm+","+r.bobNm, "a DM is listed as alice,Bob with display names")
	require.Contains(t, out, "team "+teamNm)
	out = r.kb(t, r.alice, "chat", "read", "@"+r.bobNm, "-n", "5")
	require.Contains(t, out, r.bobNm+": hi Alice", "kb chat read @Bob: %s", out)
}

func findMsg(t *testing.T, msgs []m3Msg, pred func(m3Msg) bool) m3Msg {
	t.Helper()
	for _, m := range msgs {
		if pred(m) {
			return m
		}
	}
	b, _ := json.Marshal(msgs)
	t.Fatalf("no such message in %s", b)
	return m3Msg{}
}

// typedMessages: replies, reactions, edits and deletes travel as FOKS
// pegged messages with chatd's body envelope, and come back with
// reply_to set and the envelope unwrapped, on the other chatd.
func (r *m3Rig) typedMessages(t *testing.T) {
	dmTeam := map[string]any{"kind": "adhoc", "value": r.aliceNm + "," + r.bobNm}
	def := map[string]any{"kind": "default"}
	on := func(extra map[string]any) map[string]any {
		p := map[string]any{"team": dmTeam, "app": 1, "channel": def}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	var aliceSt, bobSt struct {
		User struct {
			Party string `json:"party"`
		} `json:"user"`
	}
	r.alice.ok("status", nil, &aliceSt)
	r.bob.ok("status", nil, &bobSt)
	require.NotEmpty(t, aliceSt.User.Party, "status.user.party")
	require.NotEqual(t, aliceSt.User.Party, bobSt.User.Party)

	r.alice.ok("send", on(map[string]any{"body": "lunch at noon?"}), nil)
	r.alice.ok("send", on(map[string]any{"body": "typo mesage"}), nil)
	r.alice.ok("send", on(map[string]any{"body": "never mind"}), nil)
	merklePoke(t)
	msgs := threadMsgs(r.bob, dmTeam, def)
	lunch := findMsg(t, msgs, func(m m3Msg) bool { return m.Body == "lunch at noon?" })
	typo := findMsg(t, msgs, func(m m3Msg) bool { return m.Body == "typo mesage" })
	nevermind := findMsg(t, msgs, func(m m3Msg) bool { return m.Body == "never mind" })
	require.Equal(t, aliceSt.User.Party, lunch.Sender, "sender is the status party id")

	// bob reacts and replies; alice edits and deletes her own.
	var res struct {
		Outcome string `json:"outcome"`
	}
	r.bob.ok("react", on(map[string]any{"target": lunch.MsgID, "emoji": "👍"}), &res)
	require.Equal(t, "acked", res.Outcome)
	r.bob.ok("send", on(map[string]any{"body": "yes, the usual place", "reply_to": lunch.MsgID}), nil)
	r.alice.ok("edit", on(map[string]any{"target": typo.MsgID, "body": "typo message"}), nil)
	r.alice.ok("delete", on(map[string]any{"target": nevermind.MsgID}), nil)
	// The wire accepts an edit of someone else's message (FOKS does not
	// check); readers ignore it (kb-core's fold).
	r.bob.ok("edit", on(map[string]any{"target": lunch.MsgID, "body": "hijacked"}), nil)
	f := r.bob.call("edit", on(map[string]any{"target": lunch.MsgID, "body": " "}))
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code)
	f = r.bob.call("react", on(map[string]any{"target": "nonsense", "emoji": "👍"}))
	require.Equal(t, "bad_args", f.Error.Code)
	merklePoke(t)

	msgs = threadMsgs(r.alice, dmTeam, def)
	b, _ := json.MarshalIndent(msgs, "", " ")
	t.Logf("alice's thread view:\n%s", b)
	react := findMsg(t, msgs, func(m m3Msg) bool { return m.Typ == "reactji" })
	require.Equal(t, "👍", react.Body)
	require.NotNil(t, react.ReplyTo)
	require.Equal(t, lunch.MsgID, *react.ReplyTo)
	require.Equal(t, bobSt.User.Party, react.Sender)
	reply := findMsg(t, msgs, func(m m3Msg) bool { return m.Typ == "reply" })
	require.Equal(t, "yes, the usual place", reply.Body)
	require.Equal(t, lunch.MsgID, *reply.ReplyTo)
	edit := findMsg(t, msgs, func(m m3Msg) bool { return m.Typ == "edit" && m.Body == "typo message" })
	require.Equal(t, typo.MsgID, *edit.ReplyTo)
	del := findMsg(t, msgs, func(m m3Msg) bool { return m.Typ == "edit" && m.Body == "" })
	require.Equal(t, nevermind.MsgID, *del.ReplyTo)

	// The inbox snippet is unwrapped too (the last message is bob's
	// hijack attempt, an edit).
	row := waitInbox(t, r.alice, "the DM", func(row m3InboxRow) bool { return row.TeamKind == "adhoc" })
	require.NotNil(t, row.Snippet)
	t.Logf("snippet after the edits: %q", *row.Snippet)
	require.Equal(t, "hijacked", *row.Snippet)

	// bob takes his reaction back: a delete of the reaction message.
	r.bob.ok("delete", on(map[string]any{"target": react.MsgID}), nil)
	merklePoke(t)

	if r.kbBin == "" {
		return
	}
	// kb shows the folded thread: the edit applied, the hijack ignored,
	// the delete, the reply quote; and no reaction (taken back).
	out := r.kb(t, r.alice, "chat", "read", "@"+r.bobNm, "-n", "50", "--peek")
	t.Logf("kb chat read:\n%s", out)
	require.Contains(t, out, "alice_"+r.rnd+": lunch at noon?\n")
	require.Contains(t, out, "typo message (edited)")
	require.Contains(t, out, ": (deleted)")
	require.Contains(t, out, "(reply to [")
	require.Contains(t, out, "alice_"+r.rnd+": lunch at noon?): yes, the usual place")
	require.NotContains(t, out, "hijacked")
	require.NotContains(t, out, "👍")

	// kb chat react / edit / delete, naming messages by number.
	num := func(text string) string {
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, text) && strings.HasPrefix(line, "[") {
				return line[1:strings.Index(line, "]")]
			}
		}
		t.Fatalf("no line with %q in %s", text, out)
		return ""
	}
	lunchNum := num("lunch at noon?")
	r.kb(t, r.bob, "chat", "react", "@"+r.aliceNm, lunchNum, "🎉")
	r.kb(t, r.bob, "chat", "send", "@"+r.aliceNm, "--reply-to", lunchNum, "see you")
	r.kb(t, r.alice, "chat", "edit", "@"+r.bobNm, num("typo message"), "typo message, again")
	merklePoke(t)
	out = r.kb(t, r.alice, "chat", "read", "@"+r.bobNm, "-n", "50", "--peek")
	t.Logf("kb chat read after kb commands:\n%s", out)
	require.Contains(t, out, "🎉 1 ("+r.bobNm+")")
	require.Contains(t, out, "typo message, again (edited)")
	require.Contains(t, out, "): see you")

	// alice reacts and takes it back with --remove.
	r.kb(t, r.alice, "chat", "react", "@"+r.bobNm, lunchNum, "🙏")
	merklePoke(t)
	r.kb(t, r.alice, "chat", "react", "@"+r.bobNm, lunchNum, "🙏", "--remove")
	merklePoke(t)
	out = r.kb(t, r.alice, "chat", "read", "@"+r.bobNm, "-n", "50", "--peek")
	require.NotContains(t, out, "🙏")

	// kb chat api: reaction by message number, and read's shapes.
	api := r.kb(t, r.alice, "chat", "api", "-m", `{"method":"reaction","params":{"options":{"channel":{"name":"`+
		r.aliceNm+","+r.bobNm+`"},"message_id":`+lunchNum+`,"message":{"body":":+1:"}}}}`)
	require.Contains(t, api, `"message":"message sent"`, api)
	merklePoke(t)
	api = r.kb(t, r.alice, "chat", "api", "-m", `{"method":"read","params":{"options":{"channel":{"name":"`+
		r.aliceNm+","+r.bobNm+`"},"peek":true}}}`)
	var read struct {
		Result struct {
			Messages []struct {
				Msg struct {
					ID        int             `json:"id"`
					Content   json.RawMessage `json:"content"`
					Reactions json.RawMessage `json:"reactions"`
				} `json:"msg"`
			} `json:"messages"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal([]byte(api), &read), api)
	var found bool
	for _, m := range read.Result.Messages {
		if strconv.Itoa(m.Msg.ID) == lunchNum {
			found = true
			require.Contains(t, string(m.Msg.Reactions), ":+1:", api)
			require.Contains(t, string(m.Msg.Reactions), "🎉", api)
		}
	}
	require.True(t, found, api)
}

type m3Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Tier     string `json:"tier"`
	Archived bool   `json:"archived"`
	Roles    struct {
		Read map[string]any `json:"read"`
	} `json:"roles"`
}

func listChannels(c *chatdClient, team map[string]any) []m3Channel {
	var set struct {
		Channels []m3Channel `json:"channels"`
	}
	c.ok("list_channels_for_team", map[string]any{"team": team, "app": 1}, &set)
	return set.Channels
}

func channelNamed(t *testing.T, chans []m3Channel, name string) m3Channel {
	t.Helper()
	for _, ch := range chans {
		if ch.Name == name {
			return ch
		}
	}
	t.Fatalf("no channel %q in %+v", name, chans)
	return m3Channel{}
}

// channels: create (any member, readable by members by default), rename
// and archive (admins only: a member gets auth), archived channels refuse
// sends and can be unarchived.
func (r *m3Rig) channels(t *testing.T) {
	teamNm := "Chan_" + r.rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	team := created.Team
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)

	// alice (owner) creates a channel with the default roles: member-tier.
	var made struct {
		ID string `json:"id"`
	}
	r.alice.ok("make_channel", map[string]any{"team": team, "app": 1, "name": "design", "desc": "",
		"roles": map[string]any{"read": nil, "write": nil}}, &made)
	require.NotEmpty(t, made.ID)
	merklePoke(t)
	design := channelNamed(t, listChannels(r.alice, team), "design")
	require.Equal(t, "member", design.Roles.Read["kind"], "default roles are member: %+v", design)
	require.NotEqual(t, "Admin", design.Tier)
	byName := func(n string) map[string]any { return map[string]any{"kind": "name", "name": n} }
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": byName("design"), "body": "design kickoff"}, nil)
	merklePoke(t)
	// bob, a member, sees and reads it.
	require.Equal(t, design.ID, channelNamed(t, listChannels(r.bob, team), "design").ID)
	msgs := threadMsgs(r.bob, team, byName("design"))
	require.Len(t, msgs, 1)

	// bob may create a channel too (FOKS: any member, roles up to his own).
	r.bob.ok("make_channel", map[string]any{"team": team, "app": 1, "name": "bobs", "desc": "",
		"roles": map[string]any{"read": nil, "write": nil}}, nil)
	merklePoke(t)
	// ... but not rename or archive one: admin-only, server-enforced, auth.
	f := r.bob.call("update_channel", map[string]any{"team": team, "app": 1, "channel": byName("design"),
		"update": map[string]any{"name": "hijacked", "desc": nil, "allow_duplicate_name": false}})
	require.False(t, f.OK)
	t.Logf("member rename: %+v", f.Error)
	require.Equal(t, "auth", f.Error.Code)
	f = r.bob.call("set_channel_archived", map[string]any{"team": team, "app": 1, "channel": byName("design"), "archived": true})
	require.False(t, f.OK)
	require.Equal(t, "auth", f.Error.Code)

	// alice renames and archives.
	r.alice.ok("update_channel", map[string]any{"team": team, "app": 1, "channel": byName("design"),
		"update": map[string]any{"name": "design-2", "desc": nil, "allow_duplicate_name": false}}, nil)
	merklePoke(t)
	require.Equal(t, design.ID, channelNamed(t, listChannels(r.bob, team), "design-2").ID)
	r.alice.ok("set_channel_archived", map[string]any{"team": team, "app": 1, "channel": byName("design-2"), "archived": true}, nil)
	merklePoke(t)
	require.True(t, channelNamed(t, listChannels(r.bob, team), "design-2").Archived)
	f = r.bob.call("send", map[string]any{"team": team, "app": 1, "channel": byName("design-2"), "body": "too late"})
	t.Logf("send to an archived channel: ok=%v %+v", f.OK, f.Error)
	require.False(t, f.OK)
	require.Equal(t, "channel_archived", f.Error.Code)
	// The default channel cannot be archived (FOKS client-side check).
	f = r.alice.call("set_channel_archived", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "archived": true})
	require.False(t, f.OK)
	t.Logf("archive the default channel: %+v", f.Error)

	r.alice.ok("set_channel_archived", map[string]any{"team": team, "app": 1, "channel": byName("design-2"), "archived": false}, nil)
	merklePoke(t)
	require.False(t, channelNamed(t, listChannels(r.alice, team), "design-2").Archived)

	if r.kbBin == "" {
		return
	}
	r.kb(t, r.alice, "chat", "channel", "create", teamNm, "random", "--desc", "anything goes")
	r.kb(t, r.alice, "chat", "channel", "rename", teamNm, "random", "#off-topic")
	r.kb(t, r.alice, "chat", "channel", "archive", teamNm, "bobs")
	merklePoke(t)
	out := r.kb(t, r.bob, "chat", "channel", "ls", teamNm)
	t.Logf("kb chat channel ls:\n%s", out)
	require.Contains(t, out, "#off-topic  anything goes")
	require.Contains(t, out, "#design-2")
	require.NotContains(t, out, "#bobs")
	require.Contains(t, out, "(1 archived; --archived lists them)")
	out = r.kb(t, r.bob, "chat", "channel", "ls", teamNm, "--archived")
	require.Contains(t, out, "#bobs  [archived]")
	// A member's rename fails with kb's hint (exit 1).
	cmd := exec.Command(r.kbBin, "--no-autostart", "chat", "channel", "rename", teamNm, "off-topic", "mine")
	cmd.Env = append(os.Environ(), "KB_CHATD_SOCKET=tcp://"+r.bob.addr, "KB_CHATD_TOKEN_FILE="+r.bob.tokenFile)
	b, err := cmd.CombinedOutput()
	require.Error(t, err)
	t.Logf("kb rename as a member: %s", b)
	require.Contains(t, string(b), "only team admins")
	r.kb(t, r.alice, "chat", "channel", "unarchive", teamNm, "bobs")
}

type m3JoinRequest struct {
	RSVP string         `json:"rsvp"`
	Name string         `json:"name"`
	Host string         `json:"host"`
	Role map[string]any `json:"role"`
	Time int64          `json:"time"`
}

func joinRequests(c *chatdClient, team map[string]any) []m3JoinRequest {
	var res struct {
		Requests []m3JoinRequest `json:"requests"`
	}
	c.ok("team_join_requests", map[string]any{"team": team}, &res)
	return res.Requests
}

func memberOf(c *chatdClient, team string) bool {
	var teams struct {
		Teams []struct {
			Team map[string]string `json:"team"`
		} `json:"teams"`
	}
	c.ok("list_teams", nil, &teams)
	for _, tm := range teams.Teams {
		if strings.EqualFold(tm.Team["value"], team) {
			return true
		}
	}
	return false
}

// invitesAndJoins: alice invites, bob accepts (a join request), alice lists
// and admits it over the wire; bob is then a member. A second round goes
// through kb (team invite / accept / requests / admit --all).
func (r *m3Rig) invitesAndJoins(t *testing.T) {
	teamNm := "Inv_" + r.rnd
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": teamNm}, &created)
	team := created.Team
	merklePoke(t)
	require.Empty(t, joinRequests(r.alice, team))

	var inv struct {
		Code string `json:"code"`
	}
	r.alice.ok("create_team_invite", map[string]any{"team": team}, &inv)
	var accepted struct {
		Team map[string]string `json:"team"`
	}
	r.bob.ok("accept_team_invite", map[string]any{"code": inv.Code}, &accepted)
	require.Equal(t, teamNm, accepted.Team["value"])
	merklePoke(t)
	require.False(t, memberOf(r.bob, teamNm), "accepting only files a request")

	reqs := joinRequests(r.alice, team)
	t.Logf("join requests: %+v", reqs)
	require.Len(t, reqs, 1)
	require.Equal(t, r.bobNm, reqs[0].Name, "the requester as typed")
	require.Empty(t, reqs[0].Host, "same host")
	require.NotEmpty(t, reqs[0].RSVP)
	require.NotZero(t, reqs[0].Time)

	// Only admins see and admit requests.
	f := r.bob.call("team_join_requests", map[string]any{"team": team})
	t.Logf("join requests as a non-member: ok=%v %+v", f.OK, f.Error)
	require.False(t, f.OK)

	r.alice.ok("team_admit", map[string]any{"team": team, "requests": []map[string]any{{"rsvp": reqs[0].RSVP}}}, nil)
	merklePoke(t)
	require.Empty(t, joinRequests(r.alice, team), "admitted requests leave the list")
	var members struct {
		Members []struct {
			Name string         `json:"name"`
			Role map[string]any `json:"role"`
		} `json:"members"`
	}
	r.alice.ok("team_members", map[string]any{"team": team}, &members)
	roles := map[string]string{}
	for _, m := range members.Members {
		roles[strings.ToLower(m.Name)] = m.Role["kind"].(string)
	}
	require.Equal(t, "member", roles[strings.ToLower(r.bobNm)], "%+v", members)
	require.True(t, memberOf(r.bob, teamNm))
	// bob, now a member, reads the default channel.
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "welcome bob"}, nil)
	merklePoke(t)
	msgs := threadMsgs(r.bob, team, map[string]any{"kind": "default"})
	require.Len(t, msgs, 2)
	require.Equal(t, "welcome bob", msgs[0].Body)
	// Before it, alice's chatd's system line for the admit (PLAN-chat 63).
	require.JSONEq(t, `{"kbf":1,"sys":{"type":"added","users":["`+r.bobNm+`"],"role":"member"}}`, msgs[1].Body)
	require.Equal(t, r.aliceNm, *msgs[1].SenderName)

	if r.kbBin == "" {
		return
	}
	team2 := "Inv2_" + r.rnd
	r.kb(t, r.alice, "team", "create", team2)
	merklePoke(t)
	code := strings.TrimSpace(r.kb(t, r.alice, "team", "invite", team2))
	out := r.kb(t, r.bob, "team", "accept", code)
	require.Contains(t, out, "kb team admit")
	merklePoke(t)
	out = r.kb(t, r.alice, "team", "requests", team2)
	t.Logf("kb team requests:\n%s", out)
	require.Contains(t, out, r.bobNm+"\tuser\t")
	out = r.kb(t, r.alice, "team", "admit", team2, "--all")
	require.Contains(t, out, "Admitted "+r.bobNm+" to "+team2+".")
	merklePoke(t)
	require.True(t, memberOf(r.bob, team2))
	out = r.kb(t, r.alice, "team", "requests", team2)
	require.Contains(t, out, "No pending requests")
}
