//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives `kb fs` (WIRE.md "Files") against real chatds on the test server:
// a user's own store, a team's store read by a second member, and a
// non-member who is refused. Build chatd and kb first, then:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLIFs

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKbCLIFs(t *testing.T) {
	kbBin := os.Getenv("KB_BIN")
	if kbBin == "" {
		t.Skip("KB_BIN is not set")
	}
	r := newAttRig(t)
	defer r.close()
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, kbBin, c, "", append([]string{"fs"}, args...)...)
	}
	kbFails := func(c *chatdClient, code int, says string, args ...string) {
		t.Helper()
		out, errOut, got := kbRunCode(t, kbBin, c, "", append([]string{"fs"}, args...)...)
		require.Equal(t, code, got, "kb fs %v: stdout %q stderr %q", args, out, errOut)
		require.Contains(t, errOut, says, "kb fs %v", args)
		require.Empty(t, out, "kb fs %v printed before failing", args)
	}

	// --- your own store ---

	// The settings file makes /chat-foks in alice's store; kb fs never
	// shows it or goes into it.
	r.alice.ok("settings_set", map[string]any{"set": map[string]any{"gui.note": "kept"}}, nil)
	require.Equal(t, "", kb(r.alice, "ls", "/private"), "a new store lists empty")

	small := []byte("buy milk\nand bread\n")
	smallPath := writeTemp(t, "todo.txt", small)
	kbFails(r.alice, 1, "row_not_found", "write", smallPath, "/private/notes/todo.txt")
	out := kb(r.alice, "write", "-p", smallPath, "/private/notes/")
	t.Logf("kb fs write: %s", out)
	require.Contains(t, out, "to /private/notes/todo.txt")

	require.Equal(t, "notes/\n", kb(r.alice, "ls", "/private"))
	require.Equal(t, "todo.txt\n", kb(r.alice, "ls", "/private/notes"))
	long := kb(r.alice, "ls", "-l", "/private/notes")
	t.Logf("kb fs ls -l:\n%s", long)
	require.Regexp(t, `^f  owner   20\d\d-\d\d-\d\d \d\d:\d\d  todo\.txt\n$`, long)

	var st struct {
		Path  string `json:"path"`
		Kind  string `json:"kind"`
		Size  *int64 `json:"size"`
		Read  string `json:"read"`
		Write string `json:"write"`
	}
	require.NoError(t, json.Unmarshal([]byte(kb(r.alice, "stat", "--json", "/private/notes/todo.txt")), &st))
	t.Logf("kb fs stat --json (small): %+v", st)
	require.Equal(t, "/private/notes/todo.txt", st.Path)
	require.Equal(t, "file", st.Kind)
	require.NotNil(t, st.Size)
	require.Equal(t, int64(len(small)), *st.Size)
	require.Equal(t, "owner", st.Read)
	t.Logf("kb fs stat:\n%s", kb(r.alice, "stat", "/private/notes"))

	require.Equal(t, string(small), kb(r.alice, "read", "/private/notes/todo.txt"), "read to stdout")
	dest := filepath.Join(t.TempDir(), "todo.out")
	kb(r.alice, "read", "/private/notes/todo.txt", "-o", dest)
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.True(t, bytes.Equal(small, got))

	// A file in several KV parts (over 4 MiB), there and back.
	big := randomBytes(t, 9<<20+4321)
	bigPath := writeTemp(t, "big.bin", big)
	start := time.Now()
	out = kb(r.alice, "write", bigPath, "/private/notes/big.bin")
	t.Logf("kb fs write (%d bytes) took %v: %s", len(big), time.Since(start), out)
	st.Size = nil
	require.NoError(t, json.Unmarshal([]byte(kb(r.alice, "stat", "--json", "/private/notes/big.bin")), &st))
	require.Nil(t, st.Size, "FOKS gives no size for a file of 2 KiB or more")
	bigOut := filepath.Join(t.TempDir(), "big.out")
	start = time.Now()
	kb(r.alice, "read", "/private/notes/big.bin", "-o", bigOut)
	t.Logf("kb fs read took %v", time.Since(start))
	got, err = os.ReadFile(bigOut)
	require.NoError(t, err)
	require.True(t, bytes.Equal(big, got), "the large file came back changed (%d of %d bytes)", len(got), len(big))

	// Overwrite, and refuse to with -n.
	small2 := []byte("all done\n")
	kb(r.alice, "write", writeTemp(t, "todo.txt", small2), "/private/notes/todo.txt")
	require.Equal(t, string(small2), kb(r.alice, "read", "/private/notes/todo.txt"))
	kbFails(r.alice, 1, "already a file or folder", "write", "-n", smallPath, "/private/notes/todo.txt")

	// mv, rm.
	kb(r.alice, "mkdir", "-p", "/private/archive/2026")
	kb(r.alice, "mkdir", "-p", "/private/archive/2026")
	kbFails(r.alice, 1, "already a file or folder", "mkdir", "/private/archive")
	kb(r.alice, "mv", "/private/notes/todo.txt", "/private/archive/2026/done.txt")
	require.Equal(t, "big.bin\n", kb(r.alice, "ls", "/private/notes"))
	require.Equal(t, string(small2), kb(r.alice, "read", "/private/archive/2026/done.txt"))
	kbFails(r.alice, 1, "row_not_found", "read", "/private/notes/todo.txt")
	kbFails(r.alice, 1, "row_not_found", "mv", "/private/notes/todo.txt", "/private/x")
	// Onto a folder means into it.
	kb(r.alice, "mv", "/private/archive/2026/done.txt", "/private/notes")
	require.Equal(t, "big.bin\ndone.txt\n", kb(r.alice, "ls", "/private/notes"))
	require.Equal(t, "", kb(r.alice, "ls", "/private/archive/2026"))
	kbFails(r.alice, 1, "is a folder", "rm", "/private/archive")
	kb(r.alice, "rm", "-r", "/private/archive")
	kb(r.alice, "rm", "/private/notes/big.bin")
	kbFails(r.alice, 1, "row_not_found", "rm", "/private/notes/big.bin")
	kbFails(r.alice, 1, "row_not_found", "stat", "/private/archive/2026/done.txt")
	require.Equal(t, "notes/\n", kb(r.alice, "ls", "/private"))

	var usage struct {
		Store string `json:"store"`
		Files uint64 `json:"files"`
		Bytes uint64 `json:"bytes"`
	}
	require.NoError(t, json.Unmarshal([]byte(kb(r.alice, "usage", "--json")), &usage))
	t.Logf("kb fs usage after removing everything but the settings file: %+v", usage)
	require.Equal(t, "/private", usage.Store)

	// chatd's own folder: kb refuses before asking, and chatd refuses a
	// client that asks anyway. The settings are still there afterwards.
	kbFails(r.alice, 2, "kept for chat-foks itself", "ls", "/private/chat-foks")
	kbFails(r.alice, 2, "kept for chat-foks itself", "rm", "-r", "/private/chat-foks")
	priv := map[string]any{"kind": "private"}
	for op, params := range map[string]map[string]any{
		"kv_list": {"path": "/chat-foks"},
		"kv_stat": {"path": "/chat-foks/settings.json"},
		"kv_get":  {"path": "/chat-foks/settings.json", "dest": filepath.Join(t.TempDir(), "s.json")},
		"kv_put":  {"path": "/chat-foks/settings.json", "source": smallPath, "overwrite": true},
		"kv_rm":   {"path": "/chat-foks", "recursive": true},
		"kv_mv":   {"src": "/chat-foks", "dst": "/mine"},
	} {
		params["scope"] = priv
		f := r.alice.call(op, params)
		require.False(t, f.OK, "%s reached chatd's own folder", op)
		require.Equal(t, "rejected", f.Error.Code, "%s: %+v", op, f.Error)
	}
	f := r.alice.call("kv_rm", map[string]any{"scope": priv, "path": "/", "recursive": true})
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code)
	var set struct {
		Settings map[string]any `json:"settings"`
	}
	r.alice.ok("settings_get", nil, &set)
	require.Equal(t, "kept", set.Settings["gui.note"])

	// --- a team's store ---

	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Fs_" + r.rnd}, &created)
	team := created.Team["value"].(string)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": created.Team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)
	root := "/team/" + team

	// create_team made /chat-foks/att in the team's store: not listed.
	require.Equal(t, "", kb(r.alice, "ls", root))
	kb(r.alice, "mkdir", root+"/shared")
	plan := randomBytes(t, 1<<20+77)
	kb(r.alice, "write", writeTemp(t, "plan.bin", plan), root+"/shared/")
	require.Equal(t, "shared/\n", kb(r.alice, "ls", root))

	// The second member reads it, and may write into the folder (member
	// write), but not into the team's top folder (admins only).
	require.Equal(t, "plan.bin\n", kb(r.bob, "ls", root+"/shared"))
	t.Logf("bob: kb fs ls -l:\n%s", kb(r.bob, "ls", "-l", root+"/shared"))
	bobOut := filepath.Join(t.TempDir(), "plan.out")
	kb(r.bob, "read", root+"/shared/plan.bin", "-o", bobOut)
	got, err = os.ReadFile(bobOut)
	require.NoError(t, err)
	require.True(t, bytes.Equal(plan, got), "bob read other bytes than alice wrote")

	note := []byte("from bob\n")
	kb(r.bob, "write", writeTemp(t, "note.txt", note), root+"/shared/note.txt")
	require.Equal(t, string(note), kb(r.alice, "read", root+"/shared/note.txt"))
	kbFails(r.bob, 1, "auth: You can't change files here", "write", smallPath, root+"/top.txt")
	kbFails(r.bob, 1, "auth: You can't change files here", "mkdir", root+"/bobs")
	require.Equal(t, "note.txt\nplan.bin\n", kb(r.alice, "ls", root+"/shared"))

	// The team's store is not alice's own.
	kbFails(r.alice, 1, "row_not_found", "stat", "/private/shared")
	kbFails(r.alice, 2, "within one store", "mv", root+"/shared/note.txt", "/private/note.txt")
	require.NoError(t, json.Unmarshal([]byte(kb(r.alice, "usage", "--json", root)), &usage))
	t.Logf("kb fs usage %s: %+v", root, usage)
	require.GreaterOrEqual(t, usage.Bytes, uint64(len(plan)))

	// --- someone who is not in the team ---

	vh := vHost(t, 4)
	daveCfg := newTestAgent(t)
	dave := startChatdFresh(t, daveCfg.cfgFlags)
	dave.ok("subscribe", map[string]any{"app": 1}, nil)
	dave.signupOverWire(vh.Addr, "dave_"+r.rnd, "dave fs")
	dave.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	// A store nothing was written to yet has no top folder on the server:
	// it lists empty, and the first write makes it.
	require.Equal(t, "", kb(dave, "ls", "/private"), "a store never written to lists empty")
	t.Logf("dave: kb fs stat /private before any write:\n%s", kb(dave, "stat", "/private"))
	kb(dave, "write", smallPath, "/private/first.txt")
	require.Equal(t, "first.txt\n", kb(dave, "ls", "/private"))
	require.Equal(t, string(small), kb(dave, "read", "/private/first.txt"))
	for _, args := range [][]string{
		{"ls", root},
		{"ls", root + "/shared"},
		{"stat", root + "/shared/plan.bin"},
		{"read", root + "/shared/plan.bin"},
		{"usage", root},
	} {
		out, errOut, code := kbRunCode(t, kbBin, dave, "", append([]string{"fs"}, args...)...)
		t.Logf("dave (not a member): kb fs %s -> exit %d: %s", strings.Join(args, " "), code, strings.TrimSpace(errOut))
		require.Equal(t, 1, code, "kb fs %v", args)
		require.Empty(t, out)
		require.Contains(t, errOut, "auth: You can't read files here", "kb fs %v", args)
	}
	daveOut := filepath.Join(t.TempDir(), "plan.dave")
	kbFails(dave, 1, "auth:", "read", root+"/shared/plan.bin", "-o", daveOut)
	_, err = os.Stat(daveOut)
	require.True(t, os.IsNotExist(err), "a refused read left a file")
	kbFails(dave, 1, "auth: You can't change files here", "write", smallPath, root+"/shared/dave.txt")
	require.Equal(t, "note.txt\nplan.bin\n", kb(r.alice, "ls", root+"/shared"))
}
