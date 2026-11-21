//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. A
// deleted (or exploded) attachment goes for everyone within seconds, with
// the server's KV garbage collection off (docs/notes/25 in chat-foks):
//
//   - alice sends a file from her desk and deletes the message from her
//     laptop (a second chatd on the same account, which never saw it);
//   - bob had fetched it: his fetch says it was deleted and the copy is
//     gone from his media cache; cara never fetched it: the same;
//   - carol, a plain FOKS client in the team that kept the file's id, is
//     refused by the server;
//   - the team's usage drops only after a collection pass;
//   - the same for an exploding attachment after its time;
//   - a message alice writes by hand naming bob's attachment unlinks
//     nothing when she deletes it.
//
// Build chatd with test hooks, and kb:
//
//	CHATD_TEST_BIN=<chatd.exe built with -tags chatdtesthooks> KB_BIN=<kb.exe> \
//	    go test -tags chatfoks ./integration-tests/cli -run TestChatdDeletedAttachment -v

package cli

import (
	"encoding/hex"
	"errors"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/agent"
	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	kvStore "github.com/foks-proj/go-foks/server/kv-store"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// rawKV runs fn against carol's FOKS agent as a plain KV client acting for
// the team: what any member's own FOKS client can do, outside chatd.
func rawKV(t *testing.T, a *testAgent, teamName string, fn func(m libclient.MetaContext, cli lcl.KVClient, cfg lcl.KVConfig) error) error {
	t.Helper()
	fqt, err := core.ParseFQTeam(proto.FQTeamString(teamName))
	require.NoError(t, err)
	cfg := lcl.KVConfig{ActingAs: team.WrapNamedPtr(fqt)}
	var inner error
	// The hook runs inside the CLI's setup, as `foks kv` would, and then
	// stops the command (errRawDone).
	err = a.runCmdErr(func(m libclient.MetaContext) error {
		err := agent.Startup(m, agent.StartupOpts{})
		if err != nil {
			return err
		}
		gcli, clean, err := m.G().ConnectToAgentCli(m.Ctx())
		if err != nil {
			return err
		}
		defer clean()
		inner = fn(m, libclient.NewRpcTypedClient[lcl.KVClient](m, gcli), cfg)
		return errRawDone
	}, "ctl", "socket")
	require.ErrorIs(t, err, errRawDone)
	return inner
}

var errRawDone = errors.New("raw KV call done")

func rawList(t *testing.T, a *testAgent, teamName string, path string) []lcl.KVListEntry {
	t.Helper()
	var ents []lcl.KVListEntry
	err := rawKV(t, a, teamName, func(m libclient.MetaContext, cli lcl.KVClient, cfg lcl.KVConfig) error {
		res, err := cli.ClientKVList(m.Ctx(), lcl.ClientKVListArg{Cfg: cfg, Path: proto.KVPath(path),
			Nxt: proto.NewKVListPaginationWithNone(), Num: 100})
		ents = res.Ents
		return err
	})
	require.NoError(t, err)
	return ents
}

// keptRef is what carol keeps of an attachment's file: its KV path and id.
type keptRef struct {
	path string
	fid  proto.FileID
}

// keepRef finds an attachment's file in the team's store, as carol's own
// client can, and reads its first chunk.
func keepRef(t *testing.T, carol *testAgent, teamName, chid, attID string) keptRef {
	t.Helper()
	dir := "/chat-foks/att/" + chid + "/" + attID
	for _, e := range rawList(t, carol, teamName, dir) {
		if e.Name != "file" {
			continue
		}
		fid, err := e.Value.ToFileID()
		require.NoError(t, err)
		ref := keptRef{path: dir + "/file", fid: *fid}
		require.NoError(t, rawRead(t, carol, teamName, ref, 0), "carol reads the file while it is there")
		return ref
	}
	t.Fatalf("no file in %s", dir)
	return keptRef{}
}

// rawRead reads the chunk at off by file id. carol's agent keeps the chunks
// it read: a check after a delete reads one it never read (2 MiB).
func rawRead(t *testing.T, carol *testAgent, teamName string, ref keptRef, off int64) error {
	t.Helper()
	return rawKV(t, carol, teamName, func(m libclient.MetaContext, cli lcl.KVClient, cfg lcl.KVConfig) error {
		if off == 0 {
			_, err := cli.ClientKVGetFile(m.Ctx(), lcl.ClientKVGetFileArg{Cfg: cfg, Path: proto.KVPath(ref.path)})
			return err
		}
		_, err := cli.ClientKVGetFileChunk(m.Ctx(), lcl.ClientKVGetFileChunkArg{Cfg: cfg, Id: ref.fid, Offset: proto.Offset(off)})
		return err
	})
}

// requireRefused checks that the server refuses carol's read by id.
func requireRefused(t *testing.T, carol *testAgent, teamName string, ref keptRef) {
	t.Helper()
	err := rawRead(t, carol, teamName, ref, 2<<20)
	require.Error(t, err, "the server still serves the file")
	require.True(t, core.IsKVNoentError(err), "refused with %v", err)
}

// addLaptop signs alice in on a second chatd by key exchange from a.
func addLaptop(t *testing.T, a *chatdClient, server proto.TCPAddr, alice string) *chatdClient {
	t.Helper()
	cfg := newTestAgent(t)
	b := startChatdAt(t, cfg.cfgFlags, t.TempDir())
	b.ok("subscribe", map[string]any{"app": 1}, nil)
	var onA, onB kexStart
	a.ok("device_assist_start", nil, &onA)
	b.ok("device_login_start", map[string]any{"server": string(server), "username": alice, "device": "laptop"}, &onB)
	a.ok("device_assist_answer", map[string]any{"session": onA.Session, "code": onB.Code}, nil)
	require.True(t, a.kexDone("device_assist_done").OK)
	require.True(t, b.kexDone("device_login_done").OK)
	b.event("user_changed", 30*time.Second)
	merklePoke(t)
	merklePoke(t)
	return b
}

func TestChatdDeletedAttachment(t *testing.T) {
	kbBin := os.Getenv("KB_BIN")
	if kbBin == "" {
		t.Skip("KB_BIN is not set")
	}
	// Read by a chatd with test hooks; a release chatd ignores it and the
	// exploding part is skipped.
	t.Setenv("CHATD_TEST_EXPLODE_MIN_S", "3")
	r := newAttRig(t)
	defer r.close()
	vh := vHost(t, 4)
	carolNm := string(getActiveUser(t, r.carol).Info.Username.NameUtf8)

	// cara: a third chatd user, who never fetches anything.
	caraCfg := newTestAgent(t)
	cara := startChatdFresh(t, caraCfg.cfgFlags)
	cara.ok("subscribe", map[string]any{"app": 1}, nil)
	caraNm := "cara_" + r.rnd
	cara.signupOverWire(vh.Addr, caraNm, "cara att")
	cara.event("user_changed", 10*time.Second)
	merklePoke(t)

	var created struct {
		Team map[string]any `json:"team"`
	}
	teamName := "Del_" + r.rnd
	r.alice.ok("create_team", map[string]any{"name": teamName}, &created)
	tm := created.Team
	teamName = tm["value"].(string)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": tm, "users": []string{r.bobNm, caraNm, carolNm}}, nil)
	merklePoke(t)
	merklePoke(t)
	def := map[string]any{"kind": "default"}
	conv := func(extra map[string]any) map[string]any {
		p := map[string]any{"team": tm, "app": 1, "channel": def}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}
	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, kbBin, c, "", args...)
	}
	store := "/team/" + teamName
	type usage struct {
		Files uint64 `json:"files"`
		Bytes uint64 `json:"bytes"`
	}
	usageOf := func() usage {
		t.Helper()
		var u usage
		require.NoError(t, json.Unmarshal([]byte(kb(r.alice, "fs", "usage", store, "--json")), &u))
		return u
	}
	const grace = 2 * time.Second
	gc := func() kvStore.GCStats {
		t.Helper()
		time.Sleep(grace + time.Second)
		opts := kvStore.GCOptsFromConfig(shared.DefaultKVGCConfig())
		opts.UnlinkedGrace = grace
		st, err := kvStore.CollectGarbage(globalTestEnv.MetaContext(), opts)
		require.NoError(t, err)
		return st
	}
	fetch := func(c *chatdClient, m attMsg) chatdFrame {
		t.Helper()
		return c.call("fetch_attachment", conv(map[string]any{"msg_id": m.MsgID, "seq": m.Seq}))
	}
	requireGone := func(c *chatdClient, m attMsg, says string) {
		t.Helper()
		f := fetch(c, m)
		require.False(t, f.OK, "fetched a file that should be gone")
		require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
		require.Equal(t, says, f.Error.Message)
	}
	send := func(name string, data []byte, extra map[string]any) attMsg {
		t.Helper()
		r.alice.drainEvents()
		var res struct {
			AttID   string `json:"att_id"`
			Outcome string `json:"outcome"`
		}
		p := conv(map[string]any{"path": writeTemp(t, name, data)})
		for k, v := range extra {
			p[k] = v
		}
		r.alice.ok("send_attachment", p, &res)
		require.Equal(t, "acked", res.Outcome)
		waitUploadDone(t, r.alice, res.AttID)
		return waitAttMsg(t, r.alice, tm, def, name)
	}

	// --- deleted from another device ---

	teamBefore := usageOf()
	data := randomBytes(t, 3<<20+99)
	m := send("gone.bin", data, nil)
	bm := waitAttMsg(t, r.bob, tm, def, "gone.bin")
	cm := waitAttMsg(t, cara, tm, def, "gone.bin")
	require.Equal(t, m.MsgID, bm.MsgID)
	require.Equal(t, m.MsgID, cm.MsgID)

	// bob fetches it: it is in his media cache.
	f := fetch(r.bob, bm)
	require.True(t, f.OK, "%+v", f.Error)
	var fetched struct {
		Path string `json:"path"`
	}
	require.NoError(t, json.Unmarshal(f.Result, &fetched))
	_, err := os.Stat(fetched.Path)
	require.NoError(t, err)

	// carol finds it with her own client and keeps its id.
	atts := rawList(t, r.carol, teamName, "/chat-foks/att")
	require.Len(t, atts, 1, "one channel folder")
	chid := string(atts[0].Name)
	_, err = hex.DecodeString(chid)
	require.NoError(t, err)
	ref := keepRef(t, r.carol, teamName, chid, m.Attachment.ID)
	require.NoError(t, rawRead(t, r.carol, teamName, ref, 1<<20))
	teamWith := usageOf()
	require.GreaterOrEqual(t, teamWith.Bytes, teamBefore.Bytes+uint64(len(data)))

	// alice's laptop, which never saw the message, deletes it.
	laptop := addLaptop(t, r.alice, vh.Addr, r.aliceNm)
	r.alice.ok("send", conv(map[string]any{"body": "a line, so the laptop sees a thread"}), nil)
	merklePoke(t)
	waitAttMsg(t, laptop, tm, def, "gone.bin")
	laptop.ok("delete", conv(map[string]any{"target": m.MsgID}), nil)

	// The server refuses the file at once, with collection off.
	requireRefused(t, r.carol, teamName, ref)
	// bob has not read the delete: his chatd asks the server and drops
	// its copy.
	requireGone(r.bob, bm, "This file was deleted.")
	_, err = os.Stat(fetched.Path)
	require.True(t, os.IsNotExist(err), "bob's copy is still in his cache: %v", err)
	// cara never fetched it.
	requireGone(cara, cm, "This file was deleted.")
	// kb says so too.
	out, errOut, code := kbRunCode(t, kbBin, r.bob, "", "chat", "download", teamName, bm.MsgID)
	t.Logf("kb chat download of a deleted file: exit %d, out %q, err %q", code, out, errOut)
	require.NotEqual(t, 0, code)
	require.Contains(t, errOut, "This file was deleted.")
	// The space comes back with a collection pass, not before.
	require.Equal(t, teamWith.Bytes, usageOf().Bytes, "the delete alone frees nothing")
	st := gc()
	t.Logf("gc after the delete: %+v", st)
	require.GreaterOrEqual(t, st.Unlinked, 1)
	afterDelete := usageOf()
	require.Equal(t, teamBefore.Bytes, afterDelete.Bytes, "the deleted attachment's bytes are back")

	// --- a message written by hand naming bob's attachment ---

	r.bob.drainEvents()
	var bres struct {
		AttID string `json:"att_id"`
	}
	bdata := randomBytes(t, 2<<20+5)
	r.bob.ok("send_attachment", conv(map[string]any{"path": writeTemp(t, "bobs.bin", bdata)}), &bres)
	waitUploadDone(t, r.bob, bres.AttID)
	bobsMsg := waitAttMsg(t, r.bob, tm, def, "bobs.bin")
	bobsRef := keepRef(t, r.carol, teamName, chid, bres.AttID)
	// The envelope as chatd writes it ("kbf" first), with bob's folder,
	// file id and attachment id; the key is made up (alice has bob's from
	// his message, but the unlink does not need it).
	forgedAtt := mustJSON(t, map[string]any{
		"id": bres.AttID, "kind": "file", "name": "bobs.bin", "mime": "application/octet-stream",
		"size": len(bdata), "chunk": attPlainChunk, "key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"kv": map[string]any{"file": hex.EncodeToString(bobsRef.fid[:]), "path": bobsRef.path},
	})
	r.alice.ok("send", conv(map[string]any{"body": `{"kbf":1,"text":"","att":` + string(forgedAtt) + `}`}), nil)
	var fm attMsg
	require.Eventually(t, func() bool {
		for _, x := range attMsgs(r.alice, tm, def) {
			if x.Attachment != nil && x.Attachment.ID == bres.AttID && x.MsgID != bobsMsg.MsgID {
				fm = x
				return true
			}
		}
		merklePoke(t)
		return false
	}, time.Minute, 500*time.Millisecond, "alice's hand-written message")
	r.alice.ok("delete", conv(map[string]any{"target": fm.MsgID}), nil)
	time.Sleep(time.Second)
	require.NoError(t, rawRead(t, r.carol, teamName, bobsRef, 1<<20), "bob's file was unlinked by alice's forged message")
	f = fetch(r.bob, bobsMsg)
	require.True(t, f.OK, "%+v", f.Error)
	bc := waitAttMsg(t, cara, tm, def, "bobs.bin")
	f = fetch(cara, bc)
	require.True(t, f.OK, "%+v", f.Error)

	// --- an exploding attachment ---

	beforeExp := usageOf()
	edata := randomBytes(t, 6<<20+1)
	r.alice.drainEvents()
	var eres struct {
		AttID   string `json:"att_id"`
		Outcome string `json:"outcome"`
	}
	f = r.alice.call("send_attachment", conv(map[string]any{"path": writeTemp(t, "boom.bin", edata), "explode_after_s": 20}))
	if !f.OK {
		require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
		t.Log("this chatd has no test hooks (build it with -tags chatdtesthooks): the exploding part is skipped")
		return
	}
	require.NoError(t, json.Unmarshal(f.Result, &eres))
	require.Equal(t, "acked", eres.Outcome)
	waitUploadDone(t, r.alice, eres.AttID)
	em := waitAttMsg(t, r.alice, tm, def, "boom.bin")
	ebm := waitAttMsg(t, r.bob, tm, def, "boom.bin")
	f = fetch(r.bob, ebm)
	require.True(t, f.OK, "%+v", f.Error)
	eref := keepRef(t, r.carol, teamName, chid, em.Attachment.ID)
	require.NoError(t, rawRead(t, r.carol, teamName, eref, 1<<20))
	withExp := usageOf()
	// It explodes (bob's chatd says so by itself)...
	require.Eventually(t, func() bool {
		f := fetch(r.bob, ebm)
		return !f.OK && f.Error.Message == "This message has exploded."
	}, time.Minute, 500*time.Millisecond, "the message never exploded")
	// ...and alice's chatd unlinks it (its upload worker runs every 10
	// seconds). carol's agent keeps what it read, so each try reads a
	// chunk it has not read yet.
	refused := false
	for off := int64(2 << 20); off <= 6<<20 && !refused; off += 1 << 20 {
		time.Sleep(4 * time.Second)
		err := rawRead(t, r.carol, teamName, eref, off)
		t.Logf("carol reads the exploded file at %d: %v", off, err)
		refused = err != nil && core.IsKVNoentError(err)
	}
	require.True(t, refused, "the server still serves the exploded file")
	require.Equal(t, withExp.Bytes, usageOf().Bytes, "explosion alone frees nothing")
	st = gc()
	t.Logf("gc after the explosion: %+v", st)
	require.Equal(t, beforeExp.Bytes, usageOf().Bytes, "the exploded attachment's bytes are back")
}

func mustJSON(t *testing.T, v any) []byte {
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return b
}
