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
