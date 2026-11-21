//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives bot command adverts and custom team emoji (WIRE.md "Bot
// commands", "Custom emoji") through `kb` against real chatds on the test
// server: a member advertises commands and another member lists them, in a
// team and in a DM; an admin adds an emoji and a member fetches the same
// bytes; somebody who is not in the team is refused. Build chatd and kb
// first, then:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLICommandsEmoji

package cli

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func testPNG(t *testing.T, side int, c color.Color) []byte {
	img := image.NewRGBA(image.Rect(0, 0, side, side))
	for i := 0; i < side; i++ {
		img.Set(i, i, c)
		img.Set(side-1-i, i, c)
	}
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, img))
	return b.Bytes()
}

func TestKbCLICommandsEmoji(t *testing.T) {
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
	// api runs one `kb chat api` request and returns its result.
	api := func(c *chatdClient, method string, options map[string]any) map[string]any {
		t.Helper()
		req, err := json.Marshal(map[string]any{"method": method, "params": map[string]any{"options": options}})
		require.NoError(t, err)
		out := kb(c, "chat", "api", "-m", string(req))
		var answer struct {
			Result map[string]any `json:"result"`
			Error  map[string]any `json:"error"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &answer), "kb chat api %s: %s", method, out)
		require.Nil(t, answer.Error, "kb chat api %s: %s", method, out)
		return answer.Result
	}
	type command struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Usage       string `json:"usage"`
		Username    string `json:"username"`
		Alias       string `json:"kb_alias"`
	}
	commands := func(c *chatdClient, conv string) []command {
		t.Helper()
		var res struct {
			Commands []command `json:"commands"`
		}
		require.NoError(t, json.Unmarshal([]byte(kb(c, "chat", "commands", conv, "--json")), &res))
		return res.Commands
	}

	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Cmd_" + r.rnd}, &created)
	team := created.Team["value"].(string)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": created.Team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)

	// --- bot commands in a team ---

	require.Empty(t, commands(r.alice, team))
	require.Equal(t, "Nobody advertises commands here.\n", kb(r.alice, "chat", "commands", team))

	// bob, a plain member, advertises (create_team made the folder
	// chat-foks keeps a team's data in, with member write).
	res := api(r.bob, "advertisecommands", map[string]any{
		"alias": "Helper",
		"advertisements": []any{map[string]any{
			"type": "teamconvs", "team_name": team,
			"commands": []any{
				map[string]any{"name": "weather", "description": "The weather now", "usage": "<city>"},
				map[string]any{"name": "!help", "description": "What I can do"},
			},
		}},
	})
	require.Empty(t, res)

	got := commands(r.alice, team)
	t.Logf("alice: kb chat commands --json: %+v", got)
	require.Len(t, got, 2)
	require.Equal(t, command{Name: "help", Description: "What I can do", Username: r.bobNm, Alias: "Helper"}, got[0])
	require.Equal(t, command{Name: "weather", Description: "The weather now", Usage: "<city>", Username: r.bobNm, Alias: "Helper"}, got[1])
	plain := kb(r.alice, "chat", "commands", team)
	t.Logf("alice: kb chat commands:\n%s", plain)
	require.Contains(t, plain, "!weather <city>  The weather now  [Helper ("+r.bobNm+")]")
	// The keybase method lists the same.
	listed := api(r.alice, "listcommands", map[string]any{"channel": map[string]any{"name": team, "members_type": "team"}})
	require.Len(t, listed["commands"], 2)

	// alice advertises too; each sees both, by name and then by who.
	api(r.alice, "advertisecommands", map[string]any{"advertisements": []any{map[string]any{
		"type": "teamconvs", "team_name": team,
		"commands": []any{map[string]any{"name": "help", "description": "Ask alice"}},
	}}})
	got = commands(r.bob, team)
	require.Len(t, got, 3)
	require.Equal(t, []string{"help", "help", "weather"}, []string{got[0].Name, got[1].Name, got[2].Name})
	require.Equal(t, []string{r.aliceNm, r.bobNm}, []string{got[0].Username, got[1].Username})

	// Advertising again replaces one's own; alice's chatd, which has
	// bob's file cached, sees the change.
	api(r.bob, "advertisecommands", map[string]any{"advertisements": []any{map[string]any{
		"type": "teamconvs", "team_name": team,
		"commands": []any{map[string]any{"name": "ping", "description": "Pong"}},
	}}})
	got = commands(r.alice, team)
	t.Logf("alice after bob advertised again: %+v", got)
	require.Len(t, got, 2)
	require.Equal(t, command{Name: "ping", Description: "Pong", Username: r.bobNm}, got[1])

	// What kb has no place for is refused with a reason.
	out := kb(r.bob, "chat", "api", "-m", `{"method":"advertisecommands","params":{"options":{"advertisements":[{"type":"public","commands":[{"name":"help"}]}]}}}`)
	require.Contains(t, out, `"error"`)
	require.Contains(t, out, "no place every user can read")

	// Clear takes back only one's own.
	api(r.bob, "clearcommands", map[string]any{"filter": map[string]any{"type": "teamconvs", "team_name": team}})
	got = commands(r.alice, team)
	require.Len(t, got, 1)
	require.Equal(t, r.aliceNm, got[0].Username)
	api(r.alice, "clearcommands", map[string]any{})
	require.Empty(t, commands(r.bob, team))

	// --- bot commands in a DM (an ad-hoc team has a store too) ---

	var dm struct {
		Team map[string]any `json:"team"`
	}
	r.bob.ok("start_dm", map[string]any{"users": []string{r.aliceNm}}, &dm)
	merklePoke(t)
	r.bob.ok("commands_advertise", map[string]any{"team": dm.Team, "advertisements": []any{
		map[string]any{"commands": []any{map[string]any{"name": "remind", "usage": "<when> <what>"}}},
	}}, nil)
	var aliceDM struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("start_dm", map[string]any{"users": []string{r.bobNm}}, &aliceDM)
	var dmList struct {
		Commands []command `json:"commands"`
	}
	r.alice.ok("commands_list", map[string]any{"team": aliceDM.Team}, &dmList)
	t.Logf("alice: commands in the DM with bob: %+v", dmList.Commands)
	require.Len(t, dmList.Commands, 1)
	require.Equal(t, "remind", dmList.Commands[0].Name)
	require.Equal(t, r.bobNm, dmList.Commands[0].Username)
	// The team's list is not the DM's.
	require.Empty(t, commands(r.alice, team))
	r.bob.ok("commands_clear", map[string]any{"team": dm.Team}, nil)
	r.alice.ok("commands_list", map[string]any{"team": aliceDM.Team}, &dmList)
	require.Empty(t, dmList.Commands)

	// --- custom emoji ---

	type emoji struct {
		Alias     string  `json:"alias"`
		Etag      string  `json:"etag"`
		Mime      string  `json:"mime"`
		AddedBy   string  `json:"added_by"`
		IsAliasOf *string `json:"is_alias_of"`
	}
	emojis := func(c *chatdClient) []emoji {
		t.Helper()
		var list []emoji
		require.NoError(t, json.Unmarshal([]byte(kb(c, "team", "emoji", "ls", team, "--json")), &list))
		return list
	}
	red := testPNG(t, 48, color.RGBA{220, 30, 30, 255})
	blue := testPNG(t, 32, color.RGBA{30, 30, 220, 255})
	redPath, bluePath := writeTemp(t, "party.png", red), writeTemp(t, "blue.png", blue)

	require.Empty(t, emojis(r.bob))
	// A member can't add; an admin (alice owns the team) can.
	kbFails(r.bob, 1, "auth: Only an admin or owner", "team", "emoji", "add", team, "party", redPath)
	out = kb(r.alice, "team", "emoji", "add", team, ":party:", redPath)
	t.Logf("alice: kb team emoji add: %s", strings.TrimSpace(out))
	require.Contains(t, out, "Added :party: to "+team)
	kbFails(r.alice, 1, "already an emoji :party:", "team", "emoji", "add", team, "party", bluePath)
	kbFails(r.alice, 2, "standard emoji", "team", "emoji", "add", team, "tada", redPath)
	kbFails(r.alice, 1, "PNG, GIF or JPEG", "team", "emoji", "add", team, "words", writeTemp(t, "a.png", []byte("not a picture")))

	// A member lists it and fetches the same bytes.
	list := emojis(r.bob)
	t.Logf("bob: kb team emoji ls --json: %+v", list)
	require.Len(t, list, 1)
	require.Equal(t, "party", list[0].Alias)
	require.Equal(t, "image/png", list[0].Mime)
	require.Equal(t, r.aliceNm, list[0].AddedBy)
	require.Len(t, list[0].Etag, 32)
	fetch := func(c *chatdClient, alias string) []byte {
		t.Helper()
		dest := filepath.Join(t.TempDir(), alias+".out")
		kb(c, "team", "emoji", "get", team, alias, dest)
		data, err := os.ReadFile(dest)
		require.NoError(t, err)
		return data
	}
	require.True(t, bytes.Equal(red, fetch(r.bob, "party")), "bob fetched other bytes than alice added")
	cached := strings.TrimSpace(kb(r.bob, "team", "emoji", "get", team, "party"))
	t.Logf("bob: chatd's cached copy: %s", cached)
	require.Equal(t, "emoji.png", filepath.Base(cached))
	require.Contains(t, filepath.Base(filepath.Dir(cached)), list[0].Etag)

	// An alias shows the same picture.
	out = kb(r.alice, "team", "emoji", "alias", team, "fiesta", "party")
	require.Equal(t, ":fiesta: now shows :party: in "+team+".\n", out)
	list = emojis(r.bob)
	require.Len(t, list, 2)
	require.Equal(t, "fiesta", list[0].Alias)
	require.NotNil(t, list[0].IsAliasOf)
	require.Equal(t, "party", *list[0].IsAliasOf)
	require.Equal(t, list[1].Etag, list[0].Etag)
	require.True(t, bytes.Equal(red, fetch(r.bob, "fiesta")))
	t.Logf("bob: kb team emoji ls:\n%s", kb(r.bob, "team", "emoji", "ls", team))
	kbFails(r.bob, 1, "auth: Only an admin or owner", "team", "emoji", "alias", team, "mine", "party")
	kbFails(r.bob, 1, "auth: Only an admin or owner", "team", "emoji", "rm", team, "party")

	// Overwriting changes the picture, for the alias too.
	kb(r.alice, "team", "emoji", "add", "--overwrite", team, "party", bluePath)
	require.True(t, bytes.Equal(blue, fetch(r.bob, "fiesta")), "the alias still shows the old picture")
	after := emojis(r.bob)
	require.NotEqual(t, list[1].Etag, after[1].Etag)

	// The keybase methods see the same.
	all := api(r.bob, "emojilist", map[string]any{})
	groups := all["emojis"].(map[string]any)["emojis"].([]any)
	require.Len(t, groups, 1)
	require.Equal(t, team, groups[0].(map[string]any)["name"])
	require.Len(t, groups[0].(map[string]any)["emojis"], 2)

	// A message keeps the name as text.
	sendText(r.alice, created.Team, "done :party: :nobody:")
	merklePoke(t)
	require.Eventually(t, func() bool {
		return strings.Contains(kb(r.bob, "chat", "read", team, "--peek"), "done :party: :nobody:")
	}, 20*time.Second, 500*time.Millisecond)

	// Removing the emoji removes its alias and its image.
	out = kb(r.alice, "team", "emoji", "rm", team, "party")
	require.Equal(t, "Removed :party: and its other names (:fiesta:) from "+team+".\n", out)
	require.Empty(t, emojis(r.bob))
	kbFails(r.bob, 1, "row_not_found", "team", "emoji", "get", team, "party", filepath.Join(t.TempDir(), "gone.png"))
	kbFails(r.alice, 1, "row_not_found", "team", "emoji", "rm", team, "party")

	// --- someone who is not in the team ---

	api(r.bob, "advertisecommands", map[string]any{"advertisements": []any{map[string]any{
		"type": "teamconvs", "team_name": team, "commands": []any{map[string]any{"name": "ping"}},
	}}})
	kb(r.alice, "team", "emoji", "add", team, "party", redPath)
	vh := vHost(t, 4)
	daveCfg := newTestAgent(t)
	dave := startChatdFresh(t, daveCfg.cfgFlags)
	dave.ok("subscribe", map[string]any{"app": 1}, nil)
	dave.signupOverWire(vh.Addr, "dave_"+r.rnd, "dave data")
	dave.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	daveOut := filepath.Join(t.TempDir(), "party.dave")
	for _, args := range [][]string{
		{"chat", "commands", team},
		{"team", "emoji", "ls", team},
		{"team", "emoji", "get", team, "party", daveOut},
		{"team", "emoji", "add", team, "mine", redPath},
		{"team", "emoji", "rm", team, "party"},
	} {
		out, errOut, code := kbRunCode(t, kbBin, dave, "", args...)
		t.Logf("dave (not a member): kb %s -> exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(errOut))
		require.Equal(t, 1, code, "kb %v", args)
		require.Empty(t, out)
		require.Contains(t, errOut, "auth:", "kb %v", args)
	}
	_, err := os.Stat(daveOut)
	require.True(t, os.IsNotExist(err), "a refused fetch left a file")
	f := dave.call("commands_advertise", map[string]any{"team": created.Team, "advertisements": []any{
		map[string]any{"commands": []any{map[string]any{"name": "evil"}}},
	}})
	require.False(t, f.OK, "someone outside the team advertised in it")
	t.Logf("dave: commands_advertise -> %s: %s", f.Error.Code, f.Error.Message)
	require.Equal(t, "auth", f.Error.Code)
	f = dave.call("commands_clear", map[string]any{"team": created.Team})
	require.False(t, f.OK)
	require.Equal(t, "auth", f.Error.Code)
	// Nothing of it changed anything.
	require.Len(t, commands(r.alice, team), 1)
	require.Len(t, emojis(r.bob), 1)
}
