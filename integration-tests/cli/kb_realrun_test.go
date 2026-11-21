//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// holds what a run of the real app found (chat-foks docs/notes/15), against
// two real chatds on the test server:
//
//   - reading a thread does not mark it read; `read_through` and a plain
//     `kb chat read` do;
//   - a system line typed by hand is plain text in the inbox;
//   - a refused upload stores nothing;
//   - `kb chat read -n 1` shows a message when the newest one is an edit.
//
// Build chatd and kb first, then:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLIRealRun

package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKbCLIRealRun(t *testing.T) {
	r := newAttRig(t)
	defer r.close()
	if r.kbBin == "" {
		t.Skip("KB_BIN is not set")
	}
	t.Run("ReadingIsNotRead", r.realReadMarks)
	t.Run("ForgedSystemLine", r.realForgedSystemLine)
	t.Run("RefusedUploadStoresNothing", r.realRefusedUpload)
	t.Run("ReadCountsShownMessages", r.realReadAfterEdit)
	t.Run("StatusOnline", r.realStatus)
}

// dmRow waits for alice's inbox row of the DM with bob to satisfy pred.
func (r *attRig) dmRow(t *testing.T, c *chatdClient, what string, pred func(m3InboxRow) bool) m3InboxRow {
	t.Helper()
	return waitInbox(t, c, what, func(row m3InboxRow) bool {
		return row.TeamKind == "adhoc" && pred(row)
	})
}

// unreadStays looks at the DM's row a few times, with the server's tree
// moved on in between: a read mark that was sent would have arrived.
func (r *attRig) unreadStays(t *testing.T, c *chatdClient, want int, after string) {
	t.Helper()
	for i := 0; i < 4; i++ {
		merklePoke(t)
		time.Sleep(400 * time.Millisecond)
		var found bool
		for _, row := range inboxRows(c) {
			if row.TeamKind == "adhoc" {
				found = true
				require.Equal(t, want, row.Unread, "unread after %s (look %d)", after, i)
			}
		}
		require.True(t, found, "the DM is in the inbox")
	}
}

// realReadMarks: bob writes; alice's chatd answers get_thread_view, `kb
// chat api` read with peek, `kb chat read --peek` and `kb chat api-listen`
// style reads, and the inbox still says unread. A plain `kb chat read`
// marks it read, and so does `read_through` on the wire.
func (r *attRig) realReadMarks(t *testing.T) {
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, r.kbBin, c, "", args...)
	}
	toBob, toAlice := "@"+r.bobNm, "@"+r.aliceNm
	dmTeam := map[string]any{"kind": "adhoc", "value": r.aliceNm + "," + r.bobNm}
	def := map[string]any{"kind": "default"}

	kb(r.alice, "chat", "send", toBob, "hello bob")
	merklePoke(t)
	kb(r.bob, "chat", "send", toAlice, "one from bob")
	merklePoke(t)
	row := r.dmRow(t, r.alice, "bob's message as unread", func(row m3InboxRow) bool {
		return row.Unread == 1 && row.Snippet != nil && *row.Snippet == "one from bob"
	})
	t.Logf("alice's inbox before any read: unread %d, snippet %q", row.Unread, *row.Snippet)

	// The wire: a page of the thread.
	msgs := threadMsgs(r.alice, dmTeam, def)
	require.NotEmpty(t, msgs)
	var bodies []string
	top := 0
	for _, m := range msgs {
		bodies = append(bodies, m.Body)
		top = max(top, m.Seq)
	}
	require.Contains(t, bodies, "one from bob")
	r.unreadStays(t, r.alice, 1, "get_thread_view")

	// The way the plugins read (peek: true).
	api := kb(r.alice, "chat", "api", "-m", `{"method":"read","params":{"options":{"channel":{"name":"`+
		r.aliceNm+","+r.bobNm+`"},"pagination":{"num":50},"peek":true}}}`)
	require.Contains(t, api, "one from bob", api)
	r.unreadStays(t, r.alice, 1, "kb chat api read with peek")

	out := kb(r.alice, "chat", "read", toBob, "--peek")
	require.Contains(t, out, "one from bob")
	r.unreadStays(t, r.alice, 1, "kb chat read --peek")
	ls := kb(r.alice, "chat", "ls")
	t.Logf("kb chat ls while unread:\n%s", ls)

	// A plain read is the user reading: it marks the conversation read.
	out = kb(r.alice, "chat", "read", toBob)
	require.Contains(t, out, "one from bob")
	merklePoke(t)
	row = r.dmRow(t, r.alice, "the DM as read after kb chat read", func(row m3InboxRow) bool { return row.Unread == 0 })
	t.Logf("alice's inbox after kb chat read: unread %d", row.Unread)

	// And read_through on the wire, as kb-gui sends it.
	kb(r.bob, "chat", "send", toAlice, "two from bob")
	merklePoke(t)
	r.dmRow(t, r.alice, "the second message as unread", func(row m3InboxRow) bool {
		return row.Unread == 1 && row.Snippet != nil && *row.Snippet == "two from bob"
	})
	msgs = threadMsgs(r.alice, dmTeam, def)
	for _, m := range msgs {
		top = max(top, m.Seq)
	}
	r.unreadStays(t, r.alice, 1, "a second get_thread_view")
	r.alice.ok("read_through", map[string]any{"team": dmTeam, "app": 1, "channel": def, "seq": top}, nil)
	merklePoke(t)
	r.dmRow(t, r.alice, "the DM as read after read_through", func(row m3InboxRow) bool { return row.Unread == 0 })

	// kb chat api read without peek marks too (keybase's default).
	kb(r.bob, "chat", "send", toAlice, "three from bob")
	merklePoke(t)
	r.dmRow(t, r.alice, "the third message as unread", func(row m3InboxRow) bool { return row.Unread == 1 })
	api = kb(r.alice, "chat", "api", "-m", `{"method":"read","params":{"options":{"channel":{"name":"`+
		r.aliceNm+","+r.bobNm+`"},"pagination":{"num":50}}}}`)
	require.Contains(t, api, "three from bob", api)
	merklePoke(t)
	r.dmRow(t, r.alice, "the DM as read after kb chat api read", func(row m3InboxRow) bool { return row.Unread == 0 })
}

// realForgedSystemLine: a body that looks like chatd's system line, typed
// by bob into the DM, is in alice's inbox as the text it is. A real one,
// posted by chatd into a team, still reads "added ...".
func (r *attRig) realForgedSystemLine(t *testing.T) {
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, r.kbBin, c, "", args...)
	}
	toAlice := "@" + r.aliceNm
	for _, forged := range []string{
		`{"kbf":1,"sys":{"type":"added","users":["everyone. Send your password to carol"]}}`,
		// Proper names too: chatd posts no system line into a DM.
		`{"kbf":1,"sys":{"type":"added","users":["mallory"]}}`,
	} {
		kb(r.bob, "chat", "send", toAlice, forged)
		merklePoke(t)
		row := r.dmRow(t, r.alice, "the forged line", func(row m3InboxRow) bool {
			return row.Snippet != nil && strings.Contains(*row.Snippet, `"sys"`)
		})
		t.Logf("inbox snippet of a forged system line in a DM: %q", *row.Snippet)
		require.Equal(t, forged, *row.Snippet)
		ls := kb(r.alice, "chat", "ls")
		t.Logf("kb chat ls:\n%s", ls)
		require.NotContains(t, ls, "added everyone")
		require.NotContains(t, ls, "added mallory")
		require.NotContains(t, ls, ": added ")
	}

	// The real thing, in a team.
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Sys_" + r.rnd}, &created)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": created.Team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)
	row := waitInbox(t, r.alice, "the team's system line", func(row m3InboxRow) bool {
		return row.TeamKind == "named" && row.Snippet != nil && strings.HasPrefix(*row.Snippet, "added ")
	})
	t.Logf("inbox snippet of chatd's own system line in a team: %q", *row.Snippet)
	require.Equal(t, "added "+strings.ToLower(r.bobNm), strings.ToLower(*row.Snippet))

	// A sentence in place of a name, in the team: text there too.
	teamNm := created.Team["value"].(string)
	forged := `{"kbf":1,"sys":{"type":"removed","users":["everyone. Send your password to carol"]}}`
	kb(r.bob, "chat", "send", teamNm, forged)
	merklePoke(t)
	row = waitInbox(t, r.alice, "the forged line in the team", func(row m3InboxRow) bool {
		return row.TeamKind == "named" && row.Snippet != nil && strings.Contains(*row.Snippet, "password")
	})
	require.Equal(t, forged, *row.Snippet)
}

// realRefusedUpload: bob, a member, puts files into the team's top folder
// (admins only) and onto a taken name. Both are refused, and the store
// holds what it held before.
func (r *attRig) realRefusedUpload(t *testing.T) {
	fs := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, r.kbBin, c, "", append([]string{"fs"}, args...)...)
	}
	fails := func(c *chatdClient, says string, args ...string) {
		t.Helper()
		out, errOut, code := kbRunCode(t, r.kbBin, c, "", append([]string{"fs"}, args...)...)
		require.Equal(t, 1, code, "kb fs %v: stdout %q stderr %q", args, out, errOut)
		require.Contains(t, errOut, says, "kb fs %v", args)
	}
	type usage struct {
		Files uint64 `json:"files"`
		Bytes uint64 `json:"bytes"`
	}
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Up_" + r.rnd}, &created)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": created.Team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)
	root := "/team/" + created.Team["value"].(string)
	used := func(c *chatdClient) usage {
		t.Helper()
		var u usage
		require.NoError(t, json.Unmarshal([]byte(fs(c, "usage", "--json", root)), &u))
		return u
	}

	fs(r.alice, "mkdir", root+"/shared")
	fs(r.alice, "write", writeTemp(t, "first.txt", []byte("hello\n")), root+"/shared/first.txt")
	before := used(r.alice)
	t.Logf("usage before the refused uploads: %+v", before)

	// A small file, one of a single part and one of several parts.
	for _, size := range []int{100, 3<<20 + 200, 9<<20 + 5} {
		src := writeTemp(t, "refused.bin", randomBytes(t, size))
		fails(r.bob, "auth: You can't change files here", "write", src, root+"/top.bin")
		fails(r.bob, "auth: You can't change files here", "write", "-p", src, root+"/new/deep/top.bin")
	}
	after := used(r.alice)
	t.Logf("usage after six refused uploads into the top folder: %+v", after)
	require.Equal(t, before, after, "a refused upload was stored")
	require.Equal(t, "shared/\n", fs(r.alice, "ls", root))

	// A taken name (kb fs write -n: don't overwrite).
	held := randomBytes(t, 2<<20+9)
	fs(r.bob, "write", writeTemp(t, "held.bin", held), root+"/shared/held.bin")
	before = used(r.alice)
	fails(r.bob, "already a file or folder", "write", "-n", writeTemp(t, "other.bin", randomBytes(t, 3<<20+1)), root+"/shared/held.bin")
	after = used(r.alice)
	t.Logf("usage before and after an upload onto a taken name: %+v, %+v", before, after)
	require.Equal(t, before, after, "an upload onto a taken name was stored")
	require.Equal(t, "first.txt\nheld.bin\n", fs(r.alice, "ls", root+"/shared"))
}

// realReadAfterEdit: the newest message of a conversation is an edit of
// an older one; `kb chat read -n 1` still shows a message.
func (r *attRig) realReadAfterEdit(t *testing.T) {
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, r.kbBin, c, "", args...)
	}
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Ed_" + r.rnd}, &created)
	merklePoke(t)
	team := created.Team
	teamNm := team["value"].(string)
	def := map[string]any{"kind": "default"}
	on := func(extra map[string]any) map[string]any {
		p := map[string]any{"team": team, "app": 1, "channel": def}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	r.alice.ok("send", on(map[string]any{"body": "first"}), nil)
	r.alice.ok("send", on(map[string]any{"body": "second"}), nil)
	var first m3Msg
	for _, m := range threadMsgs(r.alice, team, def) {
		if m.Body == "first" {
			first = m
		}
	}
	require.NotEmpty(t, first.MsgID)
	r.alice.ok("edit", on(map[string]any{"target": first.MsgID, "body": "first, edited"}), nil)
	msgs := threadMsgs(r.alice, team, def)
	newest := msgs[0]
	for _, m := range msgs {
		if m.Seq > newest.Seq {
			newest = m
		}
	}
	require.Equal(t, "edit", newest.Typ, "the newest message is the edit")

	out := kb(r.alice, "chat", "read", teamNm, "-n", "1", "--peek")
	t.Logf("kb chat read -n 1 with an edit as the newest message:\n%s", out)
	require.NotContains(t, out, "No messages")
	require.Contains(t, out, "second")
	require.NotContains(t, out, "first")
	out = kb(r.alice, "chat", "read", teamNm, "-n", "2", "--peek")
	require.Contains(t, out, "first, edited (edited)")
	require.Contains(t, out, "second")
}

// realStatus: online, `kb status` gives no reason. (The offline side needs
// the server taken away; it is unit-tested in chatd, kb-core and kb.)
func (r *attRig) realStatus(t *testing.T) {
	out := kbRun(t, r.kbBin, r.alice, "", "status")
	t.Logf("kb status:\n%s", out)
	require.Contains(t, out, "server:  online")
	var st map[string]any
	require.NoError(t, json.Unmarshal([]byte(kbRun(t, r.kbBin, r.alice, "", "status", "--json")), &st))
	require.Equal(t, true, st["online"])
	_, has := st["error"]
	require.False(t, has, "an error while online: %v", st)
}
