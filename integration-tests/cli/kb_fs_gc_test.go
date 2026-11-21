//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// checks that removing things gives the space back once the server's KV
// garbage collection has run: `kb fs rm`, a replaced file, and an
// attachment whose message its sender deleted (chatd unlinks its files).
// The GC is off in the test server's config, so the test runs its passes
// itself, as the kv_store server's loop would. Build chatd and kb first:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLIFsFreesSpace

package cli

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	kvStore "github.com/foks-proj/go-foks/server/kv-store"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

func TestKbCLIFsFreesSpace(t *testing.T) {
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
	type usage struct {
		Files uint64 `json:"files"`
		Bytes uint64 `json:"bytes"`
	}
	usageOf := func(c *chatdClient, store string) usage {
		t.Helper()
		var u usage
		require.NoError(t, json.Unmarshal([]byte(kb(c, "usage", store, "--json")), &u))
		return u
	}
	// One pass of the server's GC. The grace for unlinked files is short
	// here (the server's default is an hour); uploads keep the default.
	const grace = 2 * time.Second
	gc := func(unlinkedGrace time.Duration) kvStore.GCStats {
		t.Helper()
		opts := kvStore.GCOptsFromConfig(shared.DefaultKVGCConfig())
		opts.UnlinkedGrace = unlinkedGrace
		st, err := kvStore.CollectGarbage(globalTestEnv.MetaContext(), opts)
		require.NoError(t, err)
		return st
	}
	// Whatever earlier tests left unlinked goes first, so the counts
	// below are this test's.
	time.Sleep(grace + time.Second)
	gc(grace)

	// --- kb fs rm ---

	before := usageOf(r.alice, "/private")
	big := randomBytes(t, 5<<20+777)
	bigPath := writeTemp(t, "big.bin", big)
	kb(r.alice, "write", "-p", bigPath, "/private/gc/big.bin")
	with := usageOf(r.alice, "/private")
	t.Logf("usage before %+v, with a %d byte file %+v", before, len(big), with)
	require.GreaterOrEqual(t, with.Bytes, before.Bytes+uint64(len(big)))

	kb(r.alice, "rm", "/private/gc/big.bin")
	require.Equal(t, with, usageOf(r.alice, "/private"), "rm alone frees nothing")
	st := gc(time.Hour)
	require.Zero(t, st.Unlinked, "collected inside the grace")
	require.Equal(t, with, usageOf(r.alice, "/private"))

	time.Sleep(grace + time.Second)
	st = gc(grace)
	t.Logf("gc after rm: %+v", st)
	require.Equal(t, 1, st.Unlinked)
	after := usageOf(r.alice, "/private")
	t.Logf("usage after rm and a GC pass %+v", after)
	require.Equal(t, before.Bytes, after.Bytes, "the removed file's bytes are back")

	// --- a replaced file ---

	kb(r.alice, "write", bigPath, "/private/gc/doc.bin")
	one := usageOf(r.alice, "/private")
	big2 := randomBytes(t, 5<<20+777)
	kb(r.alice, "write", writeTemp(t, "big2.bin", big2), "/private/gc/doc.bin")
	two := usageOf(r.alice, "/private")
	require.Greater(t, two.Bytes, one.Bytes, "the old copy still counts")
	time.Sleep(grace + time.Second)
	st = gc(grace)
	require.Equal(t, 1, st.Unlinked)
	require.Equal(t, one.Bytes, usageOf(r.alice, "/private").Bytes, "only the new copy counts")
	out := t.TempDir() + "/doc.out"
	kb(r.alice, "read", "/private/gc/doc.bin", "-o", out)
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, big2, got, "the new copy is whole")

	// --- an attachment whose message is deleted ---

	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Gc_" + r.rnd}, &created)
	team := created.Team
	store := "/team/" + team["value"].(string)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)
	def := map[string]any{"kind": "default"}

	teamBefore := usageOf(r.alice, store)
	data := randomBytes(t, 3<<20+99)
	r.alice.drainEvents()
	var res struct {
		AttID   string `json:"att_id"`
		Outcome string `json:"outcome"`
	}
	r.alice.ok("send_attachment", map[string]any{"team": team, "app": 1, "channel": def,
		"path": writeTemp(t, "gone.bin", data)}, &res)
	require.Equal(t, "acked", res.Outcome)
	waitUploadDone(t, r.alice, res.AttID)
	teamWith := usageOf(r.alice, store)
	require.GreaterOrEqual(t, teamWith.Bytes, teamBefore.Bytes+uint64(len(data)))

	// alice's chatd has seen its own message in the thread; bob sees it too.
	m := waitAttMsg(t, r.alice, team, def, "gone.bin")
	bm := waitAttMsg(t, r.bob, team, def, "gone.bin")
	require.Equal(t, m.MsgID, bm.MsgID)

	r.alice.ok("delete", map[string]any{"team": team, "app": 1, "channel": def, "target": m.MsgID}, nil)
	require.Equal(t, teamWith, usageOf(r.alice, store), "the delete alone frees nothing")
	time.Sleep(grace + time.Second)
	st = gc(grace)
	t.Logf("gc after delete: %+v", st)
	require.Equal(t, 1, st.Unlinked)
	teamAfter := usageOf(r.alice, store)
	t.Logf("team usage before %+v, with the attachment %+v, after delete and GC %+v", teamBefore, teamWith, teamAfter)
	require.Equal(t, teamBefore.Bytes, teamAfter.Bytes, "the deleted attachment's bytes are back")

	// bob never fetched it: his chatd is told the file is gone.
	f := r.bob.call("fetch_attachment", map[string]any{"team": team, "app": 1, "channel": def,
		"msg_id": bm.MsgID, "seq": bm.Seq})
	require.False(t, f.OK, "a collected file was fetched")
	t.Logf("bob's fetch of the collected file: %+v", f.Error)
	require.Equal(t, "rejected", f.Error.Code)
	require.Contains(t, f.Error.Message, "no longer on the server")
}
