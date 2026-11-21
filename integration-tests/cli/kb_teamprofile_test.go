//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives a team's description and picture (WIRE.md "Team profile": `kb team
// describe`, `avatar`, `show`) and `kb kvstore api` against real chatds on
// the test server: an admin sets them, a member reads them and is refused
// changing them, and someone outside the team is refused; then key-value
// entries with revisions between two members. Build chatd and kb first:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLITeamProfileKvstore

package cli

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// twoColorPNG is a w x h picture, left half red and right half blue.
func twoColorPNG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := color.RGBA{220, 30, 30, 255}
			if x >= w/2 {
				c = color.RGBA{30, 30, 220, 255}
			}
			img.SetRGBA(x, y, c)
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

type kvAnswer struct {
	Result *struct {
		TeamName   string   `json:"teamName"`
		Namespace  string   `json:"namespace"`
		EntryKey   string   `json:"entryKey"`
		EntryValue *string  `json:"entryValue"`
		Revision   int      `json:"revision"`
		Namespaces []string `json:"namespaces"`
		EntryKeys  []struct {
			EntryKey string `json:"entryKey"`
			Revision int    `json:"revision"`
		} `json:"entryKeys"`
	} `json:"result"`
	Error *struct {
		Code    int    `json:"code"`
		Name    string `json:"name"`
		Message string `json:"message"`
	} `json:"error"`
}

func TestKbCLITeamProfileKvstore(t *testing.T) {
	kbBin := os.Getenv("KB_BIN")
	if kbBin == "" {
		t.Skip("KB_BIN is not set")
	}
	r := newAttRig(t)
	defer r.close()
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, kbBin, c, "", args...)
	}
	kbFails := func(c *chatdClient, code int, says string, args ...string) {
		t.Helper()
		out, errOut, got := kbRunCode(t, kbBin, c, "", args...)
		require.Equal(t, code, got, "kb %v: stdout %q stderr %q", args, out, errOut)
		require.Contains(t, errOut, says, "kb %v", args)
		require.Empty(t, out, "kb %v printed before failing", args)
	}

	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Tp_" + r.rnd}, &created)
	team := created.Team["value"].(string)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": created.Team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)

	// --- nothing set yet ---

	type shown struct {
		Team        string `json:"team"`
		Description string `json:"description"`
		Avatar      *struct {
			Mime string `json:"mime"`
			Size int64  `json:"size"`
			Etag string `json:"etag"`
		} `json:"avatar"`
		Role     string `json:"role"`
		Members  *int   `json:"members"`
		Channels *int   `json:"channels"`
	}
	show := func(c *chatdClient) shown {
		t.Helper()
		var s shown
		require.NoError(t, json.Unmarshal([]byte(kb(c, "team", "show", "--json", team)), &s))
		return s
	}
	s := show(r.alice)
	t.Logf("alice: kb team show --json before: %+v", s)
	require.Equal(t, "", s.Description)
	require.Nil(t, s.Avatar)
	require.Equal(t, "owner", s.Role)
	require.NotNil(t, s.Members)
	require.Equal(t, 2, *s.Members)
	require.Contains(t, kb(r.bob, "team", "describe", team), "has no description")
	require.Contains(t, kb(r.bob, "team", "avatar", team), "has no picture")
	f := r.bob.call("team_avatar", map[string]any{"team": created.Team})
	require.False(t, f.OK)
	require.Equal(t, "row_not_found", f.Error.Code)

	// --- a member is refused before anything is there ---

	pngPath := writeTemp(t, "logo.png", twoColorPNG(t, 600, 300))
	kbFails(r.bob, 1, "auth: Only a team admin or owner can change", "team", "describe", team, "bob", "was", "here")
	kbFails(r.bob, 1, "auth: Only a team admin or owner can change", "team", "avatar", team, pngPath)
	require.Equal(t, "", show(r.alice).Description, "a refused change showed up")

	// --- the admin sets both ---

	out := kb(r.alice, "team", "describe", team, "We", "build", "things.")
	t.Logf("alice: kb team describe: %s", strings.TrimSpace(out))
	require.Contains(t, out, "We build things.")
	out = kb(r.alice, "team", "avatar", team, pngPath)
	t.Logf("alice: kb team avatar: %s", strings.TrimSpace(out))
	require.Contains(t, out, "Set the picture of")
	// Not a picture, by its content.
	kbFails(r.alice, 1, "The picture must be a JPEG or PNG file", "team", "avatar", team, writeTemp(t, "notes.png", []byte("not a picture")))

	// --- a member reads both ---

	require.Equal(t, "We build things.\n", kb(r.bob, "team", "describe", team))
	s = show(r.bob)
	t.Logf("bob: kb team show --json: %+v avatar %+v", s, s.Avatar)
	require.Equal(t, "We build things.", s.Description)
	require.NotNil(t, s.Avatar)
	require.Equal(t, "image/jpeg", s.Avatar.Mime)
	require.Len(t, s.Avatar.Etag, 32)
	require.Equal(t, "member", s.Role)
	text := kb(r.bob, "team", "show", team)
	t.Logf("bob: kb team show:\n%s", text)
	require.Contains(t, text, "description: We build things.")
	require.Contains(t, text, "picture:     yes")

	avatarPath := strings.TrimSpace(kb(r.bob, "team", "avatar", team))
	t.Logf("bob: kb team avatar -> %s", avatarPath)
	pic, err := os.ReadFile(avatarPath)
	require.NoError(t, err)
	require.Equal(t, s.Avatar.Size, int64(len(pic)))
	cfg, format, err := image.DecodeConfig(bytes.NewReader(pic))
	require.NoError(t, err)
	require.Equal(t, "jpeg", format)
	require.Equal(t, 256, cfg.Width, "the middle square of 600x300, scaled to 256")
	require.Equal(t, 256, cfg.Height)
	img, err := jpeg.Decode(bytes.NewReader(pic))
	require.NoError(t, err)
	red, _, blue, _ := img.At(20, 128).RGBA()
	require.Greater(t, red>>8, uint32(180), "the left of the square is red")
	require.Less(t, blue>>8, uint32(80))
	// Asked again with the etag, chatd answers from its cache.
	var av struct {
		Path string `json:"path"`
		Etag string `json:"etag"`
		Size int64  `json:"size"`
	}
	start := time.Now()
	r.bob.ok("team_avatar", map[string]any{"team": created.Team, "etag": s.Avatar.Etag}, &av)
	t.Logf("bob: team_avatar with the etag took %v", time.Since(start))
	require.Equal(t, avatarPath, av.Path)
	require.Equal(t, s.Avatar.Etag, av.Etag)

	// --- a member still can't change them; the files are admin-write ---

	kbFails(r.bob, 1, "auth: Only a team admin or owner can change", "team", "describe", team, "mine", "now")
	kbFails(r.bob, 1, "auth: Only a team admin or owner can change", "team", "describe", team, "--clear")
	kbFails(r.bob, 1, "auth: Only a team admin or owner can change", "team", "avatar", team, pngPath)
	kbFails(r.bob, 1, "auth: Only a team admin or owner can change", "team", "avatar", team, "--clear")
	require.Equal(t, "We build things.\n", kb(r.alice, "team", "describe", team))
	require.NotNil(t, show(r.alice).Avatar)
	// The folder is chatd's own: no file op reaches it.
	scope := map[string]any{"kind": "team", "value": team}
	for op, params := range map[string]map[string]any{
		"kv_list": {"path": "/chat-foks/team"},
		"kv_put":  {"path": "/chat-foks/team/profile.json", "source": pngPath, "overwrite": true},
		"kv_rm":   {"path": "/chat-foks/team", "recursive": true},
	} {
		params["scope"] = scope
		f := r.bob.call(op, params)
		require.False(t, f.OK, "%s reached the profile folder", op)
		require.Equal(t, "rejected", f.Error.Code, "%s: %+v", op, f.Error)
	}

	// --- the changes are system lines in the team's chat ---

	deadline := time.Now().Add(60 * time.Second)
	var read string
	for {
		read = kb(r.bob, "chat", "read", team, "-n", "10", "--peek")
		if strings.Contains(read, "changed the team's picture") || time.Now().After(deadline) {
			break
		}
		merklePoke(t)
		time.Sleep(500 * time.Millisecond)
	}
	t.Logf("bob: kb chat read:\n%s", read)
	require.Contains(t, read, "changed the team's description")
	require.Contains(t, read, "changed the team's picture")

	// --- someone who is not in the team ---

	vh := vHost(t, 4)
	daveCfg := newTestAgent(t)
	dave := startChatdFresh(t, daveCfg.cfgFlags)
	dave.ok("subscribe", map[string]any{"app": 1}, nil)
	dave.signupOverWire(vh.Addr, "dave_"+r.rnd, "dave tp")
	dave.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	for _, args := range [][]string{
		{"team", "show", team},
		{"team", "describe", team},
		{"team", "avatar", team},
		{"team", "describe", team, "dave", "was", "here"},
		{"team", "avatar", team, pngPath},
	} {
		out, errOut, code := kbRunCode(t, kbBin, dave, "", args...)
		t.Logf("dave (not a member): kb %s -> exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(errOut))
		require.Equal(t, 1, code, "kb %v", args)
		require.Empty(t, out)
		require.Contains(t, errOut, "auth:", "kb %v", args)
	}
	f = dave.call("team_avatar", map[string]any{"team": created.Team, "etag": s.Avatar.Etag})
	require.False(t, f.OK, "dave got the picture with bob's etag")
	require.Equal(t, "auth", f.Error.Code)

	// --- the admin takes both away ---

	require.Contains(t, kb(r.alice, "team", "avatar", team, "--clear"), "Removed the picture")
	require.Contains(t, kb(r.alice, "team", "describe", team, "--clear"), "Removed the description")
	s = show(r.bob)
	require.Equal(t, "", s.Description)
	require.Nil(t, s.Avatar)
	require.Contains(t, kb(r.bob, "team", "avatar", team), "has no picture")
	// And sets a description again: the folder is reused.
	kb(r.alice, "team", "describe", team, "Second", "try")
	require.Equal(t, "Second try\n", kb(r.bob, "team", "describe", team))

	// --- kvstore: entries with revisions, between two members ---

	api := func(c *chatdClient, method string, options map[string]any) kvAnswer {
		t.Helper()
		req, err := json.Marshal(map[string]any{"method": method, "params": map[string]any{"options": options}})
		require.NoError(t, err)
		out, errOut, code := kbRunCode(t, kbBin, c, "", "kvstore", "api", "-m", string(req))
		require.Equal(t, 0, code, "kb kvstore api %s: %s", req, errOut)
		var a kvAnswer
		require.NoError(t, json.Unmarshal([]byte(out), &a), "kb kvstore api %s answered %q", req, out)
		return a
	}
	entry := func(extra map[string]any) map[string]any {
		o := map[string]any{"team": team, "namespace": "pw-manager", "entryKey": "geocities"}
		for k, v := range extra {
			o[k] = v
		}
		return o
	}
	okRev := func(a kvAnswer, rev int) {
		t.Helper()
		require.Nil(t, a.Error, "%+v", a.Error)
		require.NotNil(t, a.Result)
		require.Equal(t, rev, a.Result.Revision)
	}
	failsWith := func(a kvAnswer, code int) {
		t.Helper()
		require.NotNil(t, a.Error, "expected error %d, got %+v", code, a.Result)
		require.Equal(t, code, a.Error.Code, "%+v", a.Error)
	}

	a := api(r.bob, "get", entry(nil))
	okRev(a, 0)
	require.Nil(t, a.Result.EntryValue)
	require.Equal(t, team, a.Result.TeamName)

	// A team's top folder is the admins': a member can't make /kvstore.
	a = api(r.bob, "put", entry(map[string]any{"entryValue": "bob first"}))
	require.NotNil(t, a.Error, "a member made the team's /kvstore folder")
	t.Logf("bob: put before an admin wrote: %+v", *a.Error)
	require.Equal(t, "auth", a.Error.Name)

	okRev(api(r.alice, "put", entry(map[string]any{"entryValue": "all my secrets", "revision": 1})), 1)
	a = api(r.bob, "get", entry(nil))
	okRev(a, 1)
	require.Equal(t, "all my secrets", *a.Result.EntryValue)

	// The member writes the next revision; the admin's stale one is refused.
	okRev(api(r.bob, "put", entry(map[string]any{"entryValue": "bob's turn"})), 2)
	a = api(r.alice, "put", entry(map[string]any{"entryValue": "stale", "revision": 2}))
	failsWith(a, 2760)
	t.Logf("alice: put with a stale revision: %+v", *a.Error)
	a = api(r.alice, "put", entry(map[string]any{"entryValue": "exists", "revision": 1}))
	failsWith(a, 2760)
	okRev(api(r.alice, "put", entry(map[string]any{"entryValue": "three", "revision": 3})), 3)
	a = api(r.bob, "get", entry(nil))
	okRev(a, 3)
	require.Equal(t, "three", *a.Result.EntryValue)

	// A second key and a second namespace, by the member.
	okRev(api(r.bob, "put", map[string]any{"team": team, "namespace": "pw-manager", "entryKey": "alta/vista 100%", "entryValue": "x"}), 1)
	okRev(api(r.bob, "put", map[string]any{"team": team, "namespace": "bot state", "entryKey": "k", "entryValue": "y"}), 1)
	a = api(r.alice, "list", map[string]any{"team": team, "namespace": "pw-manager"})
	require.Nil(t, a.Error, "%+v", a.Error)
	t.Logf("alice: list pw-manager: %+v", a.Result.EntryKeys)
	require.Len(t, a.Result.EntryKeys, 2)
	require.Equal(t, "alta/vista 100%", a.Result.EntryKeys[0].EntryKey)
	require.Equal(t, 1, a.Result.EntryKeys[0].Revision)
	require.Equal(t, "geocities", a.Result.EntryKeys[1].EntryKey)
	require.Equal(t, 3, a.Result.EntryKeys[1].Revision)
	a = api(r.alice, "list", map[string]any{"team": team})
	require.Nil(t, a.Error, "%+v", a.Error)
	require.Equal(t, []string{"bot state", "pw-manager"}, a.Result.Namespaces)
	// The entries are files, as kb fs shows them.
	require.Equal(t, "alta%2Fvista 100%25\ngeocities\n", kb(r.bob, "fs", "ls", "/team/"+team+"/kvstore/pw-manager"))

	// del: a wrong revision, then the right one; then it is gone.
	failsWith(api(r.bob, "del", entry(map[string]any{"revision": 3})), 2760)
	okRev(api(r.bob, "del", entry(map[string]any{"revision": 4})), 4)
	a = api(r.alice, "get", entry(nil))
	okRev(a, 4)
	require.Nil(t, a.Result.EntryValue)
	a = api(r.alice, "del", entry(nil))
	failsWith(a, 2762)
	t.Logf("alice: del of a deleted entry: %+v", *a.Error)
	failsWith(api(r.alice, "del", map[string]any{"team": team, "namespace": "pw-manager", "entryKey": "never"}), 2762)
	a = api(r.bob, "list", map[string]any{"team": team, "namespace": "pw-manager"})
	require.Len(t, a.Result.EntryKeys, 1)
	okRev(api(r.alice, "put", entry(map[string]any{"entryValue": "back"})), 5)

	// Several requests on stdin: one answer a line.
	stdin := `{"id": 1, "method": "get", "params": {"options": {"team": "` + team + `", "namespace": "pw-manager", "entryKey": "geocities"}}}
{"id": 2, "method": "del", "params": {"options": {"team": "` + team + `", "namespace": "pw-manager", "entryKey": "never"}}}
`
	lines := strings.Split(strings.TrimSpace(kbRun(t, kbBin, r.bob, stdin, "kvstore", "api")), "\n")
	require.Len(t, lines, 2, "%q", lines)
	require.Contains(t, lines[0], `"entryValue":"back"`)
	require.Contains(t, lines[0], `"id":1`)
	require.Contains(t, lines[1], `"code":2762`)

	// Two writers of one entry at the same moment, both naming the next
	// revision: the server takes one.
	for round := 0; round < 4; round++ {
		cur := api(r.alice, "get", entry(nil))
		require.Nil(t, cur.Error)
		next := cur.Result.Revision + 1
		var wg sync.WaitGroup
		outs := make([]string, 2)
		for i, c := range []*chatdClient{r.alice, r.bob} {
			wg.Add(1)
			go func(i int, c *chatdClient) {
				defer wg.Done()
				req, _ := json.Marshal(map[string]any{"method": "put", "params": map[string]any{"options": entry(map[string]any{
					"entryValue": []string{"alice", "bob"}[i], "revision": next})}})
				outs[i], _, _ = kbRunCode(t, kbBin, c, "", "kvstore", "api", "-m", string(req))
			}(i, c)
		}
		wg.Wait()
		won := 0
		for i, out := range outs {
			var a kvAnswer
			require.NoError(t, json.Unmarshal([]byte(out), &a), "%q", out)
			if a.Error == nil {
				won++
				require.Equal(t, next, a.Result.Revision)
			} else {
				t.Logf("round %d: writer %d lost: %+v", round, i, *a.Error)
				require.Equal(t, 2760, a.Error.Code, "%+v", a.Error)
			}
		}
		require.Equal(t, 1, won, "round %d: revision %d was given out %d times", round, next, won)
		after := api(r.bob, "get", entry(nil))
		okRev(after, next)
	}

	// Your own store is the default, and nobody else's.
	a = api(r.alice, "put", map[string]any{"namespace": "notes", "entryKey": "a", "entryValue": "mine"})
	okRev(a, 1)
	require.Equal(t, r.aliceNm+","+r.aliceNm, a.Result.TeamName)
	okRev(api(r.bob, "get", map[string]any{"namespace": "notes", "entryKey": "a"}), 0)
	a = api(r.alice, "get", map[string]any{"team": r.aliceNm + "," + r.aliceNm, "namespace": "notes", "entryKey": "a"})
	okRev(a, 1)
	require.Equal(t, "mine", *a.Result.EntryValue)

	// Someone outside the team.
	for _, method := range []string{"get", "put", "list"} {
		a = api(dave, method, entry(map[string]any{"entryValue": "dave"}))
		require.NotNil(t, a.Error, "dave: %s", method)
		t.Logf("dave (not a member): kvstore %s: %+v", method, *a.Error)
		require.Equal(t, "auth", a.Error.Name)
	}
	a = api(r.alice, "get", entry(nil))
	require.NotEqual(t, "dave", *a.Result.EntryValue)
}
