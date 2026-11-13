//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// checks chatd's settings ops (WIRE.md "Settings"): values go to the user's
// own FOKS KV store and come back after a restart. Run from a chat-foks
// checkout:
//
//	go test -tags chatfoks ./integration-tests/cli -run TestChatdV3Settings

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestChatdV3Settings(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	cfg := newTestAgent(t)
	home := t.TempDir()
	c := startChatdAt(t, cfg.cfgFlags, home)
	c.ok("subscribe", map[string]any{"app": 1}, nil)
	alice := "alice_" + strings.ToLower(fsRandomString(t, 6))
	c.signupOverWire(vh.Addr, alice, "chatd device")
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)

	type settings struct {
		Settings map[string]any `json:"settings"`
	}
	var got settings
	c.ok("settings_get", nil, &got)
	require.Empty(t, got.Settings, "a new user has none")

	c.ok("settings_set", map[string]any{"set": map[string]any{
		"chat.conv.rtch1test.mute": true,
		"linkpreview.mode":         "never",
	}}, &got)
	require.Equal(t, true, got.Settings["chat.conv.rtch1test.mute"])

	// A long one too (over FOKS's 2 KiB small-file size).
	long := strings.Repeat("x", 3000)
	c.ok("settings_set", map[string]any{"set": map[string]any{"gui.note": long}}, nil)

	// Restart chatd on the same home: its KV minder is new and keeps no
	// cache, so the values come from the FOKS server.
	c.stop()
	c = startChatdAt(t, cfg.cfgFlags, home)
	got = settings{}
	c.ok("settings_get", nil, &got)
	require.Equal(t, map[string]any{
		"chat.conv.rtch1test.mute": true,
		"linkpreview.mode":         "never",
		"gui.note":                 long,
	}, got.Settings)

	got = settings{} // json.Unmarshal adds to a map it is given
	c.ok("settings_get", map[string]any{"prefix": "chat."}, &got)
	require.Equal(t, map[string]any{"chat.conv.rtch1test.mute": true}, got.Settings)

	got = settings{}
	c.ok("settings_set", map[string]any{"remove": []string{"gui.note"}}, &got)
	require.NotContains(t, got.Settings, "gui.note")

	f := c.call("settings_set", map[string]any{"set": map[string]any{"nonamespace": 1}})
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code)
}
