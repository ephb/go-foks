//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo:
// devices and keys over chatd's wire (PLAN-chat items 55-58): listing the
// account's keys, a backup key, signing in on a fresh home with it,
// revoking a device, and changing the passphrase.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run TestChatdDevices

package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type wireDevice struct {
	ID         string         `json:"id"`
	Name       string         `json:"name"`
	Type       string         `json:"type"`
	Serial     int            `json:"serial"`
	Role       map[string]any `json:"role"`
	Created    int64          `json:"created"`
	ThisDevice bool           `json:"this_device"`
	Revoked    bool           `json:"revoked"`
}

func listDevices(c *chatdClient) []wireDevice {
	var res struct {
		Devices []wireDevice `json:"devices"`
	}
	c.ok("list_devices", nil, &res)
	return res.Devices
}

func deviceOfType(t *testing.T, ds []wireDevice, typ string) wireDevice {
	t.Helper()
	for _, d := range ds {
		if d.Type == typ {
			return d
		}
	}
	t.Fatalf("no %s key in %+v", typ, ds)
	return wireDevice{}
}

func TestChatdDevices(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)
	kbBin := os.Getenv("KB_BIN")

	cfg := newTestAgentWithOpts(t, agentOpts{defaultKeyEncryptionMode: "passphrase"})
	home := t.TempDir()
	c := startChatdAt(t, cfg.cfgFlags, home)
	c.ok("subscribe", map[string]any{"app": 1}, nil)
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice := "alice_" + rnd
	pp1 := "pp1-" + rnd + "-secret"
	c.signupPassphrase = pp1
	c.signupOverWire(vh.Addr, alice, "desk")
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)

	// 55: one key, this one.
	ds := listDevices(c)
	t.Logf("devices after signup: %+v", ds)
	require.Len(t, ds, 1)
	require.Equal(t, "desk", ds[0].Name)
	require.Equal(t, "computer", ds[0].Type)
	require.True(t, ds[0].ThisDevice)
	require.False(t, ds[0].Revoked)
	require.Equal(t, "owner", ds[0].Role["kind"])
	require.NotZero(t, ds[0].Created)
	desk := ds[0]

	// 57: change the passphrase. The desk keeps its key under the
	// passphrase, so after a restart only the new one unlocks it.
	pp2 := "pp2-" + rnd + "-secret"
	f := c.call("passphrase_change", map[string]any{"passphrase": " "})
	require.Equal(t, "bad_args", f.Error.Code)
	c.ok("passphrase_change", map[string]any{"passphrase": pp2}, nil)
	c.stop()
	c = startChatdAt(t, cfg.cfgFlags, home)
	st := c.status()
	require.False(t, st.User.Unlocked)
	require.Equal(t, "passphrase", *st.User.LockedBy)
	f = c.call("unlock", map[string]any{"passphrase": pp1})
	require.Equal(t, "bad_args", f.Error.Code, "the old passphrase: %+v", f.Error)
	var login struct {
		User *wireUserStatus `json:"user"`
	}
	c.ok("unlock", map[string]any{"passphrase": pp2}, &login)
	require.True(t, login.User.Unlocked)
	// Changed again: a change from a set passphrase, not a first one.
	pp3 := "pp3-" + rnd + "-secret"
	c.ok("passphrase_change", map[string]any{"passphrase": pp3}, nil)
	c.stop()
	c = startChatdAt(t, cfg.cfgFlags, home)
	f = c.call("unlock", map[string]any{"passphrase": pp2})
	require.Equal(t, "bad_args", f.Error.Code)
	c.ok("unlock", map[string]any{"passphrase": pp3}, &login)
	c.ok("subscribe", map[string]any{"app": 1}, nil)

	// A message to read later from the new device.
	var created struct {
		Team map[string]any `json:"team"`
	}
	c.ok("create_team", map[string]any{"name": "Dv_" + rnd}, &created)
	team := created.Team
	merklePoke(t)
	c.ok("send", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "before the backup"}, nil)
	merklePoke(t)

	// 58: a backup key, shown once.
	var bkp struct {
		Key string `json:"key"`
	}
	c.ok("create_backup_key", nil, &bkp)
	words := strings.Fields(bkp.Key)
	t.Logf("backup key has %d words", len(words))
	require.GreaterOrEqual(t, len(words), 6)
	merklePoke(t)
	ds = listDevices(c)
	require.Len(t, ds, 2)
	backup := deviceOfType(t, ds, "backup")
	require.False(t, backup.ThisDevice)

	// Sign in on a fresh home with it: a new device key of its own.
	cfg2 := newTestAgent(t)
	home2 := t.TempDir()
	c2 := startChatdAt(t, cfg2.cfgFlags, home2)
	c2.ok("subscribe", map[string]any{"app": 1}, nil)
	f = c2.call("login_backup", map[string]any{"key": "not a key at all", "device": "laptop", "server": string(vh.Addr)})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	require.NotContains(t, f.Error.Message, "not a key at all")
	c2.ok("login_backup", map[string]any{"key": bkp.Key, "device": "laptop", "server": string(vh.Addr)}, &login)
	require.NotNil(t, login.User)
	require.Equal(t, alice, login.User.Username)
	require.True(t, login.User.Unlocked)
	require.Equal(t, "laptop", login.User.Device)
	c2.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	ds = listDevices(c2)
	t.Logf("devices from the new one: %+v", ds)
	require.Len(t, ds, 3)
	var laptop wireDevice
	for _, d := range ds {
		if d.Name == "laptop" {
			laptop = d
		}
	}
	require.True(t, laptop.ThisDevice, "%+v", ds)
	require.Equal(t, "computer", laptop.Type)
	msgs := threadMsgs(c2, team, map[string]any{"kind": "default"})
	require.Len(t, msgs, 1)
	require.Equal(t, "before the backup", msgs[0].Body)

	// 56: revoke. Not this device; the old desk from the laptop.
	f = c2.call("revoke_device", map[string]any{"id": laptop.ID})
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	f = c2.call("revoke_device", map[string]any{"id": "garbage"})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	c2.ok("revoke_device", map[string]any{"id": desk.ID}, nil)
	merklePoke(t)
	merklePoke(t)
	ds = listDevices(c2)
	for _, d := range ds {
		if d.ID == desk.ID {
			require.True(t, d.Revoked, "%+v", d)
		}
	}
	// A running device keeps its live session until it reconnects (FOKS's
	// own TestRevokeHappyPath clears the user state first): restart the
	// desk's chatd. It can't unlock, read or send any more.
	c.stop()
	c = startChatdAt(t, cfg.cfgFlags, home)
	st = c.status()
	require.False(t, st.User.Unlocked)
	f = c.call("unlock", map[string]any{"passphrase": pp3})
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "revoked")
	c2.ok("send", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "after the revoke"}, nil)
	merklePoke(t)
	g := c.call("get_thread_view", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "before": 0, "num": 50})
	require.False(t, g.OK && strings.Contains(string(g.Result), "after the revoke"), "a revoked device reads new messages")
	f = c.call("send", map[string]any{"team": team, "app": 1, "channel": map[string]any{"kind": "default"}, "body": "from the revoked desk"})
	require.False(t, f.OK)
	t.Logf("the revoked desk: read %+v, send %+v", g.Error, f.Error)
	msgs = threadMsgs(c2, team, map[string]any{"kind": "default"})
	require.Len(t, msgs, 2, "the laptop still reads and writes")

	if kbBin == "" {
		return
	}
	out := kbRun(t, kbBin, c2, "", "device", "ls")
	t.Logf("kb device ls:\n%s", out)
	require.Contains(t, out, "laptop")
	require.Contains(t, out, "this device")
	require.Contains(t, out, "revoked")
	out = kbRun(t, kbBin, c2, "", "device", "revoke", "desk")
	require.Contains(t, out, "already revoked")
}
