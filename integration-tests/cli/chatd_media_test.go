//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo.
// Media between two real chatds (PLAN-chat 79, 80, 82, 90-93): a video
// with a poster the sender made, a message with a link preview card, the
// SSRF guard in the real binary, and the media cache (Mark-of-the-Web on
// Windows, emptied on logout). No test here fetches a real site.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdMedia -v

package cli

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatdMedia(t *testing.T) {
	r := newAttRig(t)
	defer r.close()
	var dm struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("start_dm", map[string]any{"users": []string{r.bobNm}}, &dm)
	merklePoke(t)

	t.Run("Poster", func(t *testing.T) { r.mediaPoster(t, dm.Team) })
	t.Run("LinkPreview", func(t *testing.T) { r.mediaLinkPreview(t, dm.Team) })
	// Last: it logs alice out.
	t.Run("CacheAndLogout", func(t *testing.T) { r.mediaCache(t, dm.Team) })
}

var defChannel = map[string]any{"kind": "default"}

// mediaPoster: alice sends a "video" with a poster she made; bob sees the
// poster as the thumbnail and fetches it as the preview.
func (r *attRig) mediaPoster(t *testing.T, team map[string]any) {
	video := randomBytes(t, 300<<10)
	poster := writeTemp(t, "frame.png", gradientPNG(t, 1280, 720))
	var res struct {
		Outcome string `json:"outcome"`
	}
	r.alice.ok("send_attachment", map[string]any{"team": team, "app": 1, "channel": defChannel,
		"path": writeTemp(t, "clip.mp4", video), "kind": "video",
		"extras": map[string]any{"dur_ms": 4200, "poster_path": poster}}, &res)
	require.Equal(t, "acked", res.Outcome)
	merklePoke(t)
	m := waitAttMsg(t, r.bob, team, defChannel, "clip.mp4")
	a := m.Attachment
	t.Logf("bob sees: kind=%s %dx%d thumb=%dx%d preview=%+v", a.Kind, a.W, a.H, a.Thumb.W, a.Thumb.H, a.Preview)
	require.Equal(t, "video", a.Kind)
	require.Equal(t, 1280, a.W)
	require.Equal(t, 320, a.Thumb.W)
	require.NotNil(t, a.Preview)
	var fr struct {
		Path string `json:"path"`
	}
	r.bob.ok("fetch_attachment", map[string]any{"team": team, "app": 1, "channel": defChannel,
		"msg_id": m.MsgID, "which": "preview"}, &fr)
	pf, err := os.Open(fr.Path)
	require.NoError(t, err)
	cfg, err := jpeg.DecodeConfig(pf)
	_ = pf.Close()
	require.NoError(t, err)
	require.Equal(t, 1280, cfg.Width)
	r.bob.ok("fetch_attachment", map[string]any{"team": team, "app": 1, "channel": defChannel, "msg_id": m.MsgID}, &fr)
	got, _ := os.ReadFile(fr.Path)
	require.True(t, bytes.Equal(video, got))
}

func smallJPEG(t *testing.T, w, h int) string {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(x), uint8(y), 200, 255})
		}
	}
	var buf bytes.Buffer
	require.NoError(t, jpeg.Encode(&buf, img, &jpeg.Options{Quality: 60}))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

type linkMsg struct {
	MsgID       string `json:"msg_id"`
	Body        string `json:"body"`
	LinkPreview *struct {
		URL   string `json:"url"`
		Title string `json:"title"`
		Desc  string `json:"desc"`
		Site  string `json:"site"`
		Thumb *struct {
			W int `json:"w"`
		} `json:"thumb"`
	} `json:"link_preview"`
}

// mediaLinkPreview: alice sends the card her composer showed; bob gets the
// text and the card, and never fetches. The real binary refuses to fetch
// from this machine.
func (r *attRig) mediaLinkPreview(t *testing.T, team map[string]any) {
	body := "look https://example.com/post"
	card := map[string]any{"url": "https://example.com/post", "title": "Example post",
		"desc": "Made by the test", "site": "Example",
		"thumb": map[string]any{"mime": "image/jpeg", "w": 160, "h": 90, "data": smallJPEG(t, 160, 90)}}
	var res struct {
		Outcome     string `json:"outcome"`
		LinkPreview bool   `json:"link_preview"`
	}
	r.alice.ok("send", map[string]any{"team": team, "app": 1, "channel": defChannel,
		"body": body, "link_preview": card}, &res)
	require.Equal(t, "acked", res.Outcome)
	require.True(t, res.LinkPreview)
	merklePoke(t)

	var found *linkMsg
	deadline := time.Now().Add(90 * time.Second)
	for found == nil && time.Now().Before(deadline) {
		var v struct {
			Messages []linkMsg `json:"messages"`
		}
		r.bob.ok("get_thread_view", map[string]any{"team": team, "app": 1, "channel": defChannel, "before": 0, "num": 50}, &v)
		for i := range v.Messages {
			if v.Messages[i].Body == body {
				found = &v.Messages[i]
			}
		}
		if found == nil {
			merklePoke(t)
			time.Sleep(500 * time.Millisecond)
		}
	}
	require.NotNil(t, found, "bob never got the message")
	t.Logf("bob sees: %q with %+v", found.Body, found.LinkPreview)
	require.NotNil(t, found.LinkPreview)
	require.Equal(t, "Example post", found.LinkPreview.Title)
	require.Equal(t, "Example", found.LinkPreview.Site)
	require.NotNil(t, found.LinkPreview.Thumb)
	require.Equal(t, 160, found.LinkPreview.Thumb.W)

	// The SSRF guard in the real binary: this machine is never fetched.
	var pr struct {
		State  string  `json:"state"`
		Reason *string `json:"reason"`
	}
	r.alice.ok("preview_link", map[string]any{"url": "http://127.0.0.1:1/admin", "force": true}, &pr)
	t.Logf("preview_link of a local address: %+v %q", pr.State, *pr.Reason)
	require.Equal(t, "none", pr.State)
	require.Contains(t, *pr.Reason, "local network")

	if r.kbBin == "" {
		return
	}
	conv := r.aliceNm + "," + r.bobNm
	read := r.kb(t, r.bob, "chat", "read", conv, "-n", "5", "--peek")
	t.Logf("kb chat read:\n%s", read)
	require.Contains(t, read, "look https://example.com/post\n    [link: Example post — Example]")
	out := r.kb(t, r.alice, "chat", "send", conv, "see http://127.0.0.1:1/x", "--preview")
	t.Logf("kb chat send --preview: %s", out)
	require.Contains(t, out, "No link preview")
	out = r.kb(t, r.alice, "chat", "preview", "http://localhost:1/")
	t.Logf("kb chat preview: %s", out)
	require.Contains(t, out, "local network")
}

// mediaCache: alice's media cache gets Mark-of-the-Web (Windows) and is
// emptied when she logs out.
func (r *attRig) mediaCache(t *testing.T, team map[string]any) {
	m := waitAttMsg(t, r.alice, team, defChannel, "clip.mp4")
	var fr struct {
		Path string `json:"path"`
	}
	r.alice.ok("fetch_attachment", map[string]any{"team": team, "app": 1, "channel": defChannel, "msg_id": m.MsgID}, &fr)
	media := filepath.Join(r.aliceDir, "localappdata", "chat-foks", "media")
	require.True(t, strings.HasPrefix(fr.Path, media), "cache path %s not under %s", fr.Path, media)
	if runtime.GOOS == "windows" {
		zone, err := os.ReadFile(fr.Path + ":Zone.Identifier")
		require.NoError(t, err)
		t.Logf("Zone.Identifier: %q", zone)
		require.Equal(t, "[ZoneTransfer]\r\nZoneId=3\r\n", string(zone))
	}
	r.alice.ok("logout", nil, nil)
	items, err := os.ReadDir(media)
	require.NoError(t, err)
	require.Empty(t, items, "media cache after logout")
}
