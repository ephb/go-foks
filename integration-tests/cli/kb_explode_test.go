//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives the kb CLI against two real chatds on the test server: a message
// to a DM nobody opened yet, `kb chat dm` with a text in place of a name,
// and exploding messages (WIRE.md "Exploding messages"). Build chatd and
// kb first, then:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLIExploding
//
// The shortest lifetime is 30 seconds. With a chatd built with
// `-tags chatdtesthooks` the test sets CHATD_TEST_EXPLODE_MIN_S and
// watches a message explode after 3 seconds; with a release chatd that
// part is skipped and said so in the log.

package cli

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKbCLIExploding(t *testing.T) {
	kbBin := os.Getenv("KB_BIN")
	if kbBin == "" {
		t.Skip("KB_BIN is not set")
	}
	// Read by a chatd with test hooks; a release chatd ignores it.
	t.Setenv("CHATD_TEST_EXPLODE_MIN_S", "3")
	r := newAttRig(t)
	defer r.close()
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, kbBin, c, "", args...)
	}
	toBob, toAlice := "@"+r.bobNm, "@"+r.aliceNm

	// --- a DM nobody has opened yet ---

	// A user who does not exist: a clear error, nothing sent.
	out, errOut, code := kbRunCode(t, kbBin, r.alice, "", "chat", "send", "@nobody_"+r.rnd, "hi")
	t.Logf("kb chat send to a missing user: exit %d, stderr %q", code, errOut)
	require.NotEqual(t, 0, code)
	require.Empty(t, out)
	require.Contains(t, errOut, "there is no DM with nobody_"+r.rnd+" yet")

	// `kb chat dm bob "some text"`: the text is not a second user.
	out, errOut, code = kbRunCode(t, kbBin, r.alice, "", "chat", "dm", r.bobNm, "hello there")
	t.Logf("kb chat dm with a text: exit %d, stderr %q", code, errOut)
	require.Equal(t, 2, code)
	require.Empty(t, out)
	require.Contains(t, errOut, "use -m")

	// The first message to bob opens the DM (it was "team not found").
	out = kb(r.alice, "chat", "send", toBob, "hello", "bob")
	t.Logf("kb chat send to a new DM: %s", out)
	require.Contains(t, out, "Sent to ")
	waitRead := func(c *chatdClient, conv, want string) string {
		t.Helper()
		var got string
		require.Eventually(t, func() bool {
			got = kb(c, "chat", "read", conv, "--peek")
			return strings.Contains(got, want)
		}, 30*time.Second, 500*time.Millisecond, "kb chat read %s never showed %q; last:\n%s", conv, want, got)
		return got
	}
	waitRead(r.bob, toAlice, "hello bob")

	// --- an exploding message, before its time ---

	out, errOut, code = kbRunCode(t, kbBin, r.alice, "", "chat", "send", toBob, "--explode", "10s", "x")
	require.Equal(t, 2, code, "stdout %q stderr %q", out, errOut)
	require.Contains(t, errOut, "from 30s to 7d")

	out = kb(r.alice, "chat", "send", toBob, "--explode", "5m", "the code is 1234")
	t.Logf("kb chat send --explode 5m:\n%s", out)
	require.Contains(t, out, "It explodes 5m after it is delivered")
	require.Contains(t, out, "The server keeps an encrypted copy.")

	read := waitRead(r.bob, toAlice, "the code is 1234")
	t.Logf("bob's kb chat read:\n%s", read)
	require.Regexp(t, `\(explodes in [45]m\): the code is 1234`, read)
	// The inbox line never has the text.
	ls := kb(r.bob, "chat", "ls")
	t.Logf("bob's kb chat ls:\n%s", ls)
	require.Contains(t, ls, "exploding message")
	require.NotContains(t, ls, "1234")

	// On the wire: explodes_at is the server's time plus the lifetime.
	dm := map[string]any{"kind": "adhoc", "value": r.aliceNm + "," + r.bobNm}
	type row struct {
		Seq        int    `json:"seq"`
		Body       string `json:"body"`
		InsertTime int64  `json:"insert_time"`
		ExplodesAt int64  `json:"explodes_at"`
		Exploded   bool   `json:"exploded"`
	}
	thread := func(c *chatdClient) []row {
		var v struct {
			Messages []row `json:"messages"`
		}
		c.ok("get_thread_view", map[string]any{"team": dm, "app": 1,
			"channel": map[string]any{"kind": "default"}, "before": 0, "num": 50}, &v)
		return v.Messages
	}
	find := func(rows []row, body string) *row {
		for i := range rows {
			if rows[i].Body == body {
				return &rows[i]
			}
		}
		return nil
	}
	got := find(thread(r.bob), "the code is 1234")
	require.NotNil(t, got)
	require.Equal(t, got.InsertTime+300_000, got.ExplodesAt)
	require.False(t, got.Exploded)

	// kb chat api: Keybase's field names.
	api := func(c *chatdClient, conv string) []map[string]any {
		t.Helper()
		req, err := json.Marshal(map[string]any{"method": "read", "params": map[string]any{"options": map[string]any{
			"channel": map[string]any{"name": conv, "members_type": "impteamnative"}, "peek": true}}})
		require.NoError(t, err)
		var ans struct {
			Result struct {
				Messages []struct {
					Msg map[string]any `json:"msg"`
				} `json:"messages"`
			} `json:"result"`
		}
		raw := kbRun(t, kbBin, c, "", "chat", "api", "-m", string(req))
		require.NoError(t, json.Unmarshal([]byte(raw), &ans), "%s", raw)
		var msgs []map[string]any
		for _, m := range ans.Result.Messages {
			msgs = append(msgs, m.Msg)
		}
		return msgs
	}
	bobConv := r.aliceNm + "," + r.bobNm
	var ephemeral map[string]any
	for _, m := range api(r.bob, bobConv) {
		if m["is_ephemeral"] == true {
			ephemeral = m
		}
	}
	require.NotNil(t, ephemeral, "no is_ephemeral message in kb chat api read")
	require.EqualValues(t, got.ExplodesAt, ephemeral["e_time"])
	require.Nil(t, ephemeral["is_ephemeral_expired"])

	// --- watching one explode (needs a chatd with test hooks) ---

	short := map[string]any{"team": dm, "app": 1, "channel": map[string]any{"kind": "default"},
		"body": "gone in three seconds", "explode_after_s": 3}
	f := r.alice.call("send", short)
	if !f.OK {
		require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
		require.Contains(t, f.Error.Message, "between 30 seconds and 7 days")
		t.Log("this chatd has no test hooks (build it with -tags chatdtesthooks): " +
			"the shortest lifetime is 30 s, so the expiry itself was not watched")
		return
	}
	// A reader fetches it while it is still there...
	waitRead(r.bob, toAlice, "gone in three seconds")
	// ...and four seconds on it is gone from both chatds, in every form.
	require.Eventually(t, func() bool {
		for _, c := range []*chatdClient{r.alice, r.bob} {
			rows := thread(c)
			if find(rows, "gone in three seconds") != nil {
				return false
			}
		}
		return true
	}, 20*time.Second, 500*time.Millisecond, "the message never exploded")
	for _, c := range []*chatdClient{r.alice, r.bob} {
		var exploded *row
		rows := thread(c)
		for i := range rows {
			if rows[i].Exploded {
				exploded = &rows[i]
			}
		}
		require.NotNil(t, exploded, "no exploded row: %+v", rows)
		require.Empty(t, exploded.Body)
		require.Equal(t, exploded.InsertTime+3000, exploded.ExplodesAt)
	}
	read = kb(r.bob, "chat", "read", toAlice, "--peek")
	t.Logf("bob's kb chat read after the time:\n%s", read)
	require.Contains(t, read, ": (exploded)")
	require.NotContains(t, read, "gone in three seconds")
	require.Contains(t, read, "the code is 1234", "the five-minute one is still there")
	export := kb(r.bob, "chat", "export", toAlice, "--json")
	require.NotContains(t, export, "gone in three seconds")
	require.Contains(t, export, `"exploded": true`)
	search, _, _ := kbRunCode(t, kbBin, r.bob, "", "chat", "search", "three", "seconds", "--conv", toAlice)
	require.NotContains(t, search, "gone in three seconds")
	expired := 0
	for _, m := range api(r.bob, bobConv) {
		if m["is_ephemeral_expired"] == true {
			expired++
			require.Equal(t, map[string]any{"type": "none"}, m["content"])
		}
	}
	require.Equal(t, 1, expired, "kb chat api read: one expired message")
	// No new edit of it.
	var target string
	{
		var v struct {
			Messages []struct {
				MsgID    string `json:"msg_id"`
				Exploded bool   `json:"exploded"`
			} `json:"messages"`
		}
		r.alice.ok("get_thread_view", map[string]any{"team": dm, "app": 1,
			"channel": map[string]any{"kind": "default"}, "before": 0, "num": 50}, &v)
		for _, m := range v.Messages {
			if m.Exploded {
				target = m.MsgID
			}
		}
	}
	f = r.alice.call("edit", map[string]any{"team": dm, "app": 1,
		"channel": map[string]any{"kind": "default"}, "target": target, "body": "too late"})
	require.False(t, f.OK)
	require.Equal(t, "This message has exploded.", f.Error.Message)
}
