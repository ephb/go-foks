//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo.
// Attachments over the wire (WIRE.md "Attachments", PLAN-chat 70-74)
// between two real chatds: a named team channel, a DM, the size cap, and
// an upload made while the FOKS server can't be reached that goes out
// after a restart. The KV refusals (not in the team, role too low, not
// enabled) are in integration-tests/lib chatfoks_attach_test.go.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdAttachments -v

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/stretchr/testify/require"
)

// attPlainChunk is chatd's plaintext per sealed chunk (attach.PlainChunk).
const attPlainChunk = 1<<20 - 16

type attRig struct {
	*m3Rig
	aliceDir   string
	aliceFlags []string
}

// newAttRig is newM3Rig, with alice's chatd on a home that survives a
// restart.
func newAttRig(t *testing.T) *attRig {
	stopper := runMerkleActivePoker(t)
	vh := vHost(t, 4)

	carol := newTestAgent(t)
	carol.runAgent(t)
	newUserWithAgentAtVHost(t, carol, 4)
	merklePoke(t)
	merklePoke(t)
	setVHostOpenViewership(t, carol)
	t.Cleanup(func() { carol.stop(t) })

	rnd := strings.ToLower(fsRandomString(t, 6))
	r := &attRig{m3Rig: &m3Rig{t: t, rnd: rnd, carol: carol, stopPoke: stopper, kbBin: os.Getenv("KB_BIN")}}

	aliceCfg := newTestAgent(t)
	r.aliceDir = t.TempDir()
	r.aliceFlags = aliceCfg.cfgFlags
	r.alice = startChatdAt(t, r.aliceFlags, r.aliceDir)
	r.alice.ok("subscribe", map[string]any{"app": 1}, nil)
	r.aliceNm = "alice_" + rnd
	r.alice.signupOverWire(vh.Addr, r.aliceNm, "alice att")
	r.alice.event("user_changed", 10*time.Second)

	bobCfg := newTestAgent(t)
	r.bob = startChatdFresh(t, bobCfg.cfgFlags)
	r.bob.ok("subscribe", map[string]any{"app": 1}, nil)
	r.bobNm = "bob_" + rnd
	r.bob.signupOverWire(vh.Addr, r.bobNm, "bob att")
	r.bob.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	return r
}

type attView struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	W      int    `json:"w"`
	H      int    `json:"h"`
	Cached bool   `json:"cached"`
	Thumb  *struct {
		W    int    `json:"w"`
		H    int    `json:"h"`
		Data string `json:"data"`
	} `json:"thumb"`
	Preview *struct {
		W int `json:"w"`
		H int `json:"h"`
	} `json:"preview"`
}

type attMsg struct {
	MsgID      string   `json:"msg_id"`
	Seq        int      `json:"seq"`
	Typ        string   `json:"typ"`
	Body       string   `json:"body"`
	Attachment *attView `json:"attachment"`
}

func attMsgs(c *chatdClient, team, channel map[string]any) []attMsg {
	var v struct {
		Messages []attMsg `json:"messages"`
	}
	c.ok("get_thread_view", map[string]any{"team": team, "app": 1, "channel": channel, "before": 0, "num": 50}, &v)
	return v.Messages
}

// waitAttMsg polls c's thread until a message with an attachment named
// name shows up.
func waitAttMsg(t *testing.T, c *chatdClient, team, channel map[string]any, name string) attMsg {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range attMsgs(c, team, channel) {
			if m.Attachment != nil && m.Attachment.Name == name {
				return m
			}
		}
		merklePoke(t)
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("no message with attachment %s", name)
	return attMsg{}
}

// waitUploadDone reads c's events until att's upload is done.
func waitUploadDone(t *testing.T, c *chatdClient, attID string) {
	t.Helper()
	deadline := time.After(2 * time.Minute)
	for {
		select {
		case f := <-c.events:
			if f.Event != "attachment_progress" {
				continue
			}
			var p struct {
				AttID string `json:"att_id"`
				State string `json:"state"`
				Done  int64  `json:"done"`
				Total int64  `json:"total"`
			}
			require.NoError(t, json.Unmarshal(f.Data, &p))
			if p.AttID == attID && p.State != "running" {
				require.Equal(t, "done", p.State, "progress %s", f.Data)
				require.Equal(t, p.Total, p.Done)
				return
			}
		case <-deadline:
			t.Fatalf("no attachment_progress done for %s", attID)
		}
	}
}

func writeTemp(t *testing.T, name string, data []byte) string {
	p := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(p, data, 0o600))
	return p
}

func randomBytes(t *testing.T, n int) []byte {
	b := make([]byte, n)
	require.NoError(t, core.RandomFill(b))
	return b
}

func TestChatdAttachments(t *testing.T) {
	r := newAttRig(t)
	defer r.close()

	t.Run("NamedTeam", r.attNamedTeam)
	t.Run("DM", r.attDM)
	t.Run("SizeCap", r.attSizeCap)
	// Last: it restarts alice's chatd.
	t.Run("WhileServerUnreachable", r.attOffline)
}

// attNamedTeam: alice (owner) creates a team, which makes its attachments
// folder; bob, a plain member, sends several MiB into its channel; alice
// reads it back whole and by offset.
func (r *attRig) attNamedTeam(t *testing.T) {
	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Att_" + r.rnd}, &created)
	team := created.Team
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)
	def := map[string]any{"kind": "default"}

	data := randomBytes(t, 6<<20+12345)
	r.bob.drainEvents()
	start := time.Now()
	var res struct {
		AttID   string `json:"att_id"`
		Outcome string `json:"outcome"`
	}
	r.bob.ok("send_attachment", map[string]any{"team": team, "app": 1, "channel": def,
		"path": writeTemp(t, "data.bin", data), "caption": "the data"}, &res)
	t.Logf("bob (member) sent %d bytes into the team in %v: %+v", len(data), time.Since(start), res)
	require.Equal(t, "acked", res.Outcome)
	waitUploadDone(t, r.bob, res.AttID)

	merklePoke(t)
	m := waitAttMsg(t, r.alice, team, def, "data.bin")
	require.Equal(t, "the data", m.Body)
	require.Equal(t, res.AttID, m.Attachment.ID)
	require.Equal(t, int64(len(data)), m.Attachment.Size)
	require.Equal(t, "file", m.Attachment.Kind)
	require.False(t, m.Attachment.Cached)

	start = time.Now()
	var fr struct {
		Path string `json:"path"`
		Size int64  `json:"size"`
	}
	r.alice.ok("fetch_attachment", map[string]any{"team": team, "app": 1, "channel": def, "msg_id": m.MsgID}, &fr)
	t.Logf("alice fetched it in %v to %s", time.Since(start), fr.Path)
	got, err := os.ReadFile(fr.Path)
	require.NoError(t, err)
	require.True(t, bytes.Equal(data, got), "fetched file differs")

	// Seek: bob (no cache of his own download) reads across a chunk edge.
	var rr struct {
		Data  string `json:"data"`
		Total int64  `json:"total"`
		EOF   bool   `json:"eof"`
	}
	off := 3*attPlainChunk - 100
	start = time.Now()
	r.bob.ok("read_attachment", map[string]any{"team": team, "app": 1, "channel": def, "msg_id": m.MsgID,
		"seq": m.Seq, "offset": off, "length": 300}, &rr)
	t.Logf("bob read 300 bytes at %d in %v", off, time.Since(start))
	b, _ := base64.StdEncoding.DecodeString(rr.Data)
	require.True(t, bytes.Equal(data[off:off+300], b))
	require.Equal(t, int64(len(data)), rr.Total)
	require.False(t, rr.EOF)
	r.bob.ok("read_attachment", map[string]any{"team": team, "app": 1, "channel": def, "msg_id": m.MsgID,
		"offset": len(data) - 10, "length": 100}, &rr)
	b, _ = base64.StdEncoding.DecodeString(rr.Data)
	require.True(t, bytes.Equal(data[len(data)-10:], b))
	require.True(t, rr.EOF)

	// Save as.
	dest := filepath.Join(t.TempDir(), "copy.bin")
	r.alice.ok("save_attachment", map[string]any{"team": team, "app": 1, "channel": def, "msg_id": m.MsgID, "dest": dest}, nil)
	got, _ = os.ReadFile(dest)
	require.True(t, bytes.Equal(data, got))

	// The inbox line for a captionless file.
	r.bob.ok("send_attachment", map[string]any{"team": team, "app": 1, "channel": def,
		"path": writeTemp(t, "notes.txt", []byte("a few words"))}, nil)
	merklePoke(t)
	row := waitInbox(t, r.alice, "the team's snippet", func(row m3InboxRow) bool {
		return row.Snippet != nil && *row.Snippet == "sent a file: notes.txt"
	})
	t.Logf("snippet: %q", *row.Snippet)

	if r.kbBin != "" {
		r.attKb(t, team["value"].(string))
	}
}

func gradientPNG(t *testing.T, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x * 255 / w), uint8(y * 255 / h), 128, 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, img))
	return buf.Bytes()
}

// attDM: alice sends a big photo into the DM with bob; bob gets the
// thumbnail in the thread, then the preview and the original.
func (r *attRig) attDM(t *testing.T) {
	var dm struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("start_dm", map[string]any{"users": []string{r.bobNm}}, &dm)
	merklePoke(t)
	def := map[string]any{"kind": "default"}
	pic := gradientPNG(t, 2400, 1600)
	var res struct {
		AttID   string `json:"att_id"`
		Outcome string `json:"outcome"`
	}
	r.alice.ok("send_attachment", map[string]any{"team": dm.Team, "app": 1, "channel": def,
		"path": writeTemp(t, "photo.png", pic), "caption": "the view"}, &res)
	require.Equal(t, "acked", res.Outcome)
	merklePoke(t)

	// bob's side of the DM names it his way; the team value alice got
	// works for him too (the same ad-hoc team).
	m := waitAttMsg(t, r.bob, dm.Team, def, "photo.png")
	a := m.Attachment
	t.Logf("bob sees: kind=%s mime=%s %dx%d thumb=%dx%d (%d b64 bytes) preview=%+v",
		a.Kind, a.Mime, a.W, a.H, a.Thumb.W, a.Thumb.H, len(a.Thumb.Data), a.Preview)
	require.Equal(t, "image", a.Kind)
	require.Equal(t, "image/png", a.Mime)
	require.Equal(t, 2400, a.W)
	require.Equal(t, 1600, a.H)
	require.Equal(t, 320, a.Thumb.W)
	require.NotNil(t, a.Preview)
	require.Equal(t, 1280, a.Preview.W)

	var fr struct {
		Path string `json:"path"`
	}
	r.bob.ok("fetch_attachment", map[string]any{"team": dm.Team, "app": 1, "channel": def,
		"msg_id": m.MsgID, "which": "preview"}, &fr)
	pf, err := os.Open(fr.Path)
	require.NoError(t, err)
	cfg, err := jpeg.DecodeConfig(pf)
	_ = pf.Close()
	require.NoError(t, err)
	require.Equal(t, 1280, cfg.Width)
	r.bob.ok("fetch_attachment", map[string]any{"team": dm.Team, "app": 1, "channel": def, "msg_id": m.MsgID}, &fr)
	got, _ := os.ReadFile(fr.Path)
	require.True(t, bytes.Equal(pic, got))
	require.True(t, waitAttMsg(t, r.bob, dm.Team, def, "photo.png").Attachment.Cached)
}

// attSizeCap: a file over 100 MiB is refused before anything is read.
func (r *attRig) attSizeCap(t *testing.T) {
	p := filepath.Join(t.TempDir(), "huge.bin")
	f, err := os.Create(p)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(100<<20+1))
	require.NoError(t, f.Close())
	var dm struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("start_dm", map[string]any{"users": []string{r.bobNm}}, &dm)
	fr := r.alice.call("send_attachment", map[string]any{"team": dm.Team, "app": 1,
		"channel": map[string]any{"kind": "default"}, "path": p})
	require.False(t, fr.OK)
	t.Logf("over the cap: %+v", fr.Error)
	require.Equal(t, "rejected", fr.Error.Code)
	require.Contains(t, fr.Error.Message, "at most 104857600 bytes")
}

// attOffline: alice's chatd can't reach the server (FOKS's
// --test-kill-network); her upload waits in chatd's queue, survives a
// restart, and goes out once the server answers again.
func (r *attRig) attOffline(t *testing.T) {
	var dm struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("start_dm", map[string]any{"users": []string{r.bobNm}}, &dm)
	def := map[string]any{"kind": "default"}

	r.alice.stop()
	r.alice = startChatdAt(t, append(append([]string{}, r.aliceFlags...), "--test-kill-network"), r.aliceDir)
	data := randomBytes(t, 2<<20+99)
	var res struct {
		AttID   string `json:"att_id"`
		Outcome string `json:"outcome"`
	}
	r.alice.ok("send_attachment", map[string]any{"team": dm.Team, "app": 1, "channel": def,
		"path": writeTemp(t, "offline.bin", data), "caption": "sent offline"}, &res)
	t.Logf("offline send_attachment: %+v", res)
	require.Equal(t, "waiting", res.Outcome)
	var lu struct {
		Uploads []struct {
			AttID string `json:"att_id"`
			State string `json:"state"`
			Error *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"uploads"`
	}
	r.alice.ok("list_uploads", nil, &lu)
	t.Logf("list_uploads offline: %+v", lu)
	require.Len(t, lu.Uploads, 1)
	require.Equal(t, res.AttID, lu.Uploads[0].AttID)
	require.Equal(t, "waiting", lu.Uploads[0].State)
	require.Equal(t, "transport", lu.Uploads[0].Error.Code)

	// Back online, after a restart: the queue file carries it.
	r.alice.stop()
	r.alice = startChatdAt(t, r.aliceFlags, r.aliceDir)
	merklePoke(t)
	m := waitAttMsg(t, r.bob, dm.Team, def, "offline.bin")
	require.Equal(t, "sent offline", m.Body)
	var fr struct {
		Path string `json:"path"`
	}
	r.bob.ok("fetch_attachment", map[string]any{"team": dm.Team, "app": 1, "channel": def, "msg_id": m.MsgID}, &fr)
	got, _ := os.ReadFile(fr.Path)
	require.True(t, bytes.Equal(data, got))
	r.alice.ok("list_uploads", nil, &lu)
	require.Empty(t, lu.Uploads)
}

// attKb: the kb CLI's upload and download against bob's and alice's
// chatds (PLAN-chat 74).
func (r *attRig) attKb(t *testing.T, team string) {
	data := randomBytes(t, 1<<20+5)
	src := writeTemp(t, "via-kb.bin", data)
	out := r.kb(t, r.bob, "chat", "upload", team, src, "--caption", "from kb")
	t.Logf("kb chat upload: %s", out)
	merklePoke(t)
	m := waitAttMsg(t, r.alice, map[string]any{"kind": "named", "value": team}, map[string]any{"kind": "default"}, "via-kb.bin")
	require.Equal(t, "from kb", m.Body)
	read := r.kb(t, r.alice, "chat", "read", team, "-n", "5", "--peek")
	t.Logf("kb chat read:\n%s", read)
	require.Contains(t, read, "via-kb.bin")
	dest := filepath.Join(t.TempDir(), "out.bin")
	out = r.kb(t, r.alice, "chat", "download", team, m.MsgID, "-o", dest)
	t.Logf("kb chat download: %s", out)
	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	require.True(t, bytes.Equal(data, got))
}
