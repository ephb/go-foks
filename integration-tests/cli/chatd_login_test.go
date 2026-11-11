//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives chatd's login, unlock and logout operations (WIRE.md "Login, unlock
// and logout") against a real chatd and test server. Run from a chat-foks
// checkout:
//
//	go test -tags chatfoks ./integration-tests/cli -run 'TestChatdV3(Login|Passphrase)'

package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type wireUserStatus struct {
	Username string  `json:"username"`
	Host     string  `json:"host"`
	Device   string  `json:"device"`
	Unlocked bool    `json:"unlocked"`
	LockedBy *string `json:"locked_by"`
}

type wireStatus struct {
	User   *wireUserStatus `json:"user"`
	Online bool            `json:"online"`
}

type wireLocalUser struct {
	Username string `json:"username"`
	Host     string `json:"host"`
	Active   bool   `json:"active"`
}

// close ends this connection (the daemon keeps running).
func (c *chatdClient) close() { _ = c.conn.Close() }

func (c *chatdClient) status() wireStatus {
	var st wireStatus
	c.ok("status", nil, &st)
	return st
}

// localUsers maps username -> active.
func (c *chatdClient) localUsers() map[string]bool {
	var res struct {
		Users []wireLocalUser `json:"users"`
	}
	c.ok("list_existing_users", nil, &res)
	ret := map[string]bool{}
	for _, u := range res.Users {
		ret[u.Username] = ret[u.Username] || u.Active
	}
	return ret
}

func (c *chatdClient) hostOf(username string) string {
	var res struct {
		Users []wireLocalUser `json:"users"`
	}
	c.ok("list_existing_users", nil, &res)
	for _, u := range res.Users {
		if u.Username == username {
			return u.Host
		}
	}
	c.t.Fatalf("%s is not a local user", username)
	return ""
}

func (c *chatdClient) login(username string) wireUserStatus {
	var res struct {
		User *wireUserStatus `json:"user"`
	}
	c.ok("login", map[string]any{"username": username, "host": c.hostOf(username)}, &res)
	require.NotNil(c.t, res.User)
	require.Equal(c.t, username, res.User.Username)
	return *res.User
}

// TestChatdV3LoginSwitchLogout: two accounts on one chatd home. Sign up
// alice, log out, sign up bob (signup now offers alice as an existing
// user), then switch between them with login and log out again; every
// change is announced with user_changed and the poll loop follows.
func TestChatdV3LoginSwitchLogout(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	cfg := newTestAgent(t)
	c := startChatdFresh(t, cfg.cfgFlags)
	c.ok("subscribe", map[string]any{"app": 1}, nil)

	rnd := strings.ToLower(fsRandomString(t, 6))
	alice, bob := "alice_"+rnd, "bob_"+rnd

	c.signupOverWire(vh.Addr, alice, "chatd device")
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	require.Equal(t, map[string]bool{alice: true}, c.localUsers())
	c.event("connectivity", 30*time.Second) // online for alice

	// logout: no active user, the account stays on the device.
	c.drainEvents()
	c.ok("logout", nil, nil)
	c.event("user_changed", 10*time.Second)
	st := c.status()
	require.Nil(t, st.User, "status after logout: %+v", st)
	require.Equal(t, map[string]bool{alice: false}, c.localUsers())
	f := c.call("list_teams", nil)
	require.False(t, f.OK, "chat ops need a user after logout")
	ev := c.event("connectivity", 30*time.Second)
	require.JSONEq(t, `{"online":false}`, string(ev.Data))
	c.ok("logout", nil, nil) // twice is fine

	// A second account; signup offers alice first (answered -1: new).
	c.signupOverWire(vh.Addr, bob, "chatd device")
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	require.Equal(t, map[string]bool{alice: false, bob: true}, c.localUsers())
	require.Equal(t, bob, c.status().User.Username)

	// Switch to alice. logout dropped her keys from memory; login loads
	// them again from the (plaintext) secret store, unlocked.
	c.drainEvents()
	u := c.login(alice)
	require.True(t, u.Unlocked, "%+v", u)
	require.Nil(t, u.LockedBy)
	c.event("user_changed", 10*time.Second)
	st = c.status()
	require.Equal(t, alice, st.User.Username)
	require.Equal(t, "chatd device", st.User.Device)
	require.Equal(t, map[string]bool{alice: true, bob: false}, c.localUsers())
	tm := "lt_" + rnd
	c.ok("create_team", map[string]any{"name": tm}, nil)
	merklePoke(t)
	var teams struct {
		Teams []struct {
			Team map[string]string `json:"team"`
		} `json:"teams"`
	}
	c.ok("list_teams", nil, &teams)
	require.Len(t, teams.Teams, 1)
	require.Equal(t, tm, teams.Teams[0].Team["value"])

	// Logging in as the active user: no-op, no event.
	c.drainEvents()
	c.login(alice)
	select {
	case f := <-c.events:
		require.NotEqual(t, "user_changed", f.Event, "login as the active user announced a change")
	case <-time.After(500 * time.Millisecond):
	}

	// Back to bob, who was loaded in this process: still unlocked, and his
	// view is his own (alice's team is not his).
	u = c.login(bob)
	require.True(t, u.Unlocked)
	c.event("user_changed", 10*time.Second)
	c.ok("list_teams", nil, &teams)
	require.Empty(t, teams.Teams, "bob sees alice's teams: %+v", teams)

	// Unknown and missing usernames.
	f = c.call("login", map[string]any{"username": "carol_" + rnd})
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	f = c.call("login", map[string]any{})
	require.False(t, f.OK)
	require.Equal(t, "invalid_input", f.Error.Code)

	// No host: the username alone is unambiguous here.
	var res struct {
		User *wireUserStatus `json:"user"`
	}
	c.ok("login", map[string]any{"username": alice}, &res)
	require.Equal(t, alice, res.User.Username)
	c.event("user_changed", 10*time.Second)

	// The poll loop serves the new user: online again.
	c.drainEvents()
	c.ok("logout", nil, nil)
	c.login(bob)
	c.event("user_changed", 10*time.Second)
	deadline := time.Now().Add(30 * time.Second)
	for !c.status().Online {
		require.True(t, time.Now().Before(deadline), "poll loop never came back online for bob")
		time.Sleep(200 * time.Millisecond)
	}
}

// TestChatdV3PassphraseUnlock: with passphrase key storage, signup asks
// for a passphrase over the wire; after a chatd restart the user is locked
// (status.user.locked_by "passphrase") and unlock opens it. login after
// logout needs the passphrase again.
func TestChatdV3PassphraseUnlock(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	cfg := newTestAgentWithOpts(t, agentOpts{defaultKeyEncryptionMode: "passphrase"})
	home := t.TempDir()
	c := startChatdAt(t, cfg.cfgFlags, home)
	c.ok("subscribe", map[string]any{"app": 1}, nil)
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice := "alice_" + rnd
	pp := "pp-" + rnd + "-secret"
	c.signupPassphrase = pp
	c.signupOverWire(vh.Addr, alice, "chatd device")
	c.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	st := c.status()
	require.True(t, st.User.Unlocked, "right after signup: %+v", st.User)

	// Restart chatd on the same home: alice comes back, locked.
	c.stop()
	c = startChatdAt(t, cfg.cfgFlags, home)
	c.ok("subscribe", map[string]any{"app": 1}, nil)
	st = c.status()
	require.NotNil(t, st.User)
	require.Equal(t, alice, st.User.Username)
	require.False(t, st.User.Unlocked)
	require.NotNil(t, st.User.LockedBy)
	require.Equal(t, "passphrase", *st.User.LockedBy)
	require.False(t, st.Online, "the poll loop idles for a locked user")
	f := c.call("list_teams", nil)
	require.False(t, f.OK, "chat ops on a locked user")

	// Wrong, then right.
	f = c.call("unlock", map[string]any{"passphrase": pp + "X"})
	require.False(t, f.OK)
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	require.NotContains(t, f.Error.Message, pp)
	c.drainEvents()
	var res struct {
		User *wireUserStatus `json:"user"`
	}
	c.ok("unlock", map[string]any{"passphrase": pp}, &res)
	require.True(t, res.User.Unlocked)
	require.Nil(t, res.User.LockedBy)
	c.event("user_changed", 10*time.Second)
	ev := c.event("connectivity", 30*time.Second)
	require.JSONEq(t, `{"online":true}`, string(ev.Data))
	c.ok("list_teams", nil, nil)

	// logout drops the keys; login brings alice back locked.
	c.ok("logout", nil, nil)
	u := c.login(alice)
	require.False(t, u.Unlocked)
	require.Equal(t, "passphrase", *u.LockedBy)
	c.ok("unlock", map[string]any{"passphrase": pp}, &res)
	require.True(t, res.User.Unlocked)
}
