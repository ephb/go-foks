//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo:
// bot tokens and removing an account from a device over chatd's wire
// (WIRE.md "Bot tokens", "Removing an account from this device") and the
// kb CLI on top of them.
//
//	CHATD_TEST_BIN=<chatd.exe> [KB_BIN=<kb.exe>] go test -tags chatfoks \
//	    ./integration-tests/cli -run 'TestChatdV3BotToken|TestKbCLIForget'

package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type wireBotToken struct {
	Token string         `json:"token"`
	Name  string         `json:"name"`
	Host  string         `json:"host"`
	Role  map[string]any `json:"role"`
}

type wireRemoved struct {
	Username  string `json:"username"`
	Host      string `json:"host"`
	WasActive bool   `json:"was_active"`
	DeviceKey bool   `json:"device_key"`
}

// filesHolding lists the files under dir that contain needle.
func filesHolding(t *testing.T, dir string, needle string) []string {
	t.Helper()
	var hits []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p)
		if err == nil && bytes.Contains(b, []byte(needle)) {
			hits = append(hits, p)
		}
		return nil
	})
	require.NoError(t, err)
	return hits
}

// kbRunEnv is kbRunCode with extra environment for kb.
func kbRunEnv(t *testing.T, bin string, c *chatdClient, env []string, stdin string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"--no-autostart"}, args...)...)
	cmd.Env = append(append(os.Environ(),
		"KB_CHATD_SOCKET=tcp://"+c.addr,
		"KB_CHATD_TOKEN_FILE="+c.tokenFile,
	), env...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		require.True(t, ok, "kb %v: %v", args, err)
		code = exitErr.ExitCode()
	}
	return out.String(), errOut.String(), code
}

func defaultChannel() map[string]any { return map[string]any{"kind": "default"} }

func sendText(c *chatdClient, team map[string]any, body string) {
	c.ok("send", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "body": body}, nil)
}

func bodies(msgs []m3Msg) []string {
	var out []string
	for _, m := range msgs {
		out = append(out, m.Body)
	}
	return out
}

// TestChatdV3BotToken: alice makes a bot token; a chatd on an empty home
// signs in with it and reads and sends in a team channel; nothing of the
// token is written to that home; after a restart the bot account is locked
// until the token is given again; once the token's key is revoked the
// server refuses it.
func TestChatdV3BotToken(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)
	kbBin := os.Getenv("KB_BIN")

	cfg := newTestAgent(t)
	home := t.TempDir()
	a := startChatdAt(t, cfg.cfgFlags, home)
	a.ok("subscribe", map[string]any{"app": 1}, nil)
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice := "alice_" + rnd
	a.signupOverWire(vh.Addr, alice, "desk")
	a.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)

	var created struct {
		Team map[string]any `json:"team"`
	}
	a.ok("create_team", map[string]any{"name": "Bt_" + rnd}, &created)
	team := created.Team
	merklePoke(t)
	sendText(a, team, "before the bot")
	merklePoke(t)

	// A token, shown once: name.key, the server to use it on, its role.
	var tok wireBotToken
	a.ok("bot_token_new", nil, &tok)
	require.Equal(t, 2, len(strings.Split(tok.Token, ".")), "token shape")
	require.True(t, strings.HasPrefix(tok.Token, tok.Name+"."))
	require.Equal(t, "owner", tok.Role["kind"])
	require.Equal(t, string(vh.Addr), tok.Host)
	secret := strings.SplitN(tok.Token, ".", 2)[1]
	merklePoke(t)
	merklePoke(t)
	ds := listDevices(a)
	require.Len(t, ds, 2)
	botKey := deviceOfType(t, ds, "bot_token")
	require.Equal(t, tok.Name, botKey.Name)
	require.False(t, botKey.ThisDevice)
	require.False(t, botKey.Revoked)
	// Making it wrote nothing of it to alice's home either.
	require.Empty(t, filesHolding(t, home, secret), "the token is on alice's disk")

	// A role is needed. Below owner FOKS has no per-user key to give the
	// token on this account ("key not found: PUK"), and chatd says so.
	f := a.call("bot_token_new", map[string]any{"role": map[string]any{"kind": "none"}})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	for _, kind := range []string{"admin", "member"} {
		f = a.call("bot_token_new", map[string]any{"role": map[string]any{"kind": kind}})
		t.Logf("a %s token: ok=%v %+v", kind, f.OK, f.Error)
		require.False(t, f.OK)
		require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
		require.Contains(t, f.Error.Message, "owner token")
	}
	require.Len(t, listDevices(a), 2, "a refused token left a key behind")

	// The bot: a chatd on an empty home.
	cfg2 := newTestAgent(t)
	home2 := t.TempDir()
	b := startChatdAt(t, cfg2.cfgFlags, home2)
	b.ok("subscribe", map[string]any{"app": 1}, nil)
	f = b.call("login_bot_token", map[string]any{"host": tok.Host, "token": "not a token"})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	require.NotContains(t, f.Error.Message, "not a token\"")
	// A well-formed token the server has never seen: one letter changed.
	flip := map[byte]byte{'a': 'b'}
	mid := len(secret) / 2
	ch, ok := flip[secret[mid]]
	if !ok {
		ch = 'a'
	}
	unknown := tok.Name + "." + secret[:mid] + string(ch) + secret[mid+1:]
	f = b.call("login_bot_token", map[string]any{"host": tok.Host, "token": unknown})
	require.False(t, f.OK)
	t.Logf("an unknown token: %+v", f.Error)
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)
	require.Nil(t, b.status().User, "a refused token signed somebody in")
	require.Empty(t, b.localUsers())

	var login struct {
		User *wireUserStatus `json:"user"`
	}
	b.ok("login_bot_token", map[string]any{"host": tok.Host, "token": tok.Token}, &login)
	require.NotNil(t, login.User)
	require.Equal(t, alice, login.User.Username)
	require.True(t, login.User.Unlocked)
	require.Equal(t, tok.Name, login.User.Device)
	b.event("user_changed", 10*time.Second)
	merklePoke(t)
	ds = listDevices(b)
	t.Logf("devices from the bot: %+v", ds)
	for _, d := range ds {
		require.Equal(t, d.ID == botKey.ID, d.ThisDevice, "%+v", d)
	}

	// It reads what was there and sends.
	require.Equal(t, []string{"before the bot"}, bodies(threadMsgs(b, team, defaultChannel())))
	sendText(b, team, "from the bot")
	merklePoke(t)
	require.Eventually(t, func() bool {
		return len(threadMsgs(a, team, defaultChannel())) == 2
	}, 30*time.Second, 300*time.Millisecond)
	require.ElementsMatch(t, []string{"before the bot", "from the bot"}, bodies(threadMsgs(a, team, defaultChannel())))

	// One-shot: the token is in chatd's memory only. Nothing under the
	// bot's home (FOKS's databases, its key store, chatd's log) holds it.
	require.Empty(t, filesHolding(t, home2, secret), "the token is on the bot's disk")
	require.Empty(t, filesHolding(t, home2, tok.Token))
	// What FOKS does write: the account, in its local list.
	require.Equal(t, map[string]bool{alice: true}, b.localUsers())

	// So after a restart the account is listed and locked, until the token
	// is given again.
	b.stop()
	b = startChatdAt(t, cfg2.cfgFlags, home2)
	st := b.status()
	require.NotNil(t, st.User)
	require.False(t, st.User.Unlocked)
	require.Equal(t, "bot_token", *st.User.LockedBy)
	f = b.call("get_thread_view", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "before": 0, "num": 50})
	require.False(t, f.OK)
	t.Logf("a locked bot reads: %+v", f.Error)
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "bot token again")
	b.ok("login_bot_token", map[string]any{"host": tok.Host, "token": tok.Token}, &login)
	require.True(t, login.User.Unlocked)
	require.Len(t, threadMsgs(b, team, defaultChannel()), 2)
	require.Empty(t, filesHolding(t, home2, secret), "the token is on the bot's disk after the second sign-in")

	if kbBin != "" {
		out := kbRun(t, kbBin, a, "", "device", "ls")
		t.Logf("kb device ls:\n%s", out)
		require.Contains(t, out, tok.Name+"\tbot token\t")
		// kb bot token new: the token once, with what to do with it.
		_, errOut, code := kbRunCode(t, kbBin, a, "", "bot", "token", "new", "--role", "member")
		require.Equal(t, 1, code, "%s", errOut)
		require.Contains(t, errOut, "owner token")
		out = kbRun(t, kbBin, a, "", "bot", "token", "new")
		t.Logf("kb bot token new:\n%s", out)
		require.Contains(t, out, "keep it secret")
		require.Contains(t, out, "kb login --bot-token --server "+tok.Host)
		var kbTok string
		for _, line := range strings.Split(out, "\n") {
			if s := strings.TrimSpace(line); len(strings.Split(s, ".")) == 2 && !strings.Contains(s, " ") && len(s) > 20 {
				kbTok = s
			}
		}
		require.NotEmpty(t, kbTok, "no token in: %s", out)

		// kb login --bot-token on a third home: from stdin, then from
		// KB_BOT_TOKEN; never as an argument.
		cfg4 := newTestAgent(t)
		home4 := t.TempDir()
		k := startChatdAt(t, cfg4.cfgFlags, home4)
		_, errOut, code = kbRunCode(t, kbBin, k, "", "login", "--bot-token", "--server", tok.Host)
		require.NotEqual(t, 0, code, "no token given")
		require.Contains(t, errOut, "KB_BOT_TOKEN")
		_, errOut, code = kbRunCode(t, kbBin, k, "nonsense\n", "login", "--bot-token", "--server", tok.Host)
		require.Equal(t, 1, code, "%s", errOut)
		require.Contains(t, errOut, "not a bot token")
		out = kbRun(t, kbBin, k, tok.Token+"\n", "login", "--bot-token", "--server", tok.Host)
		require.Contains(t, out, "Signed in as "+alice+"@")
		out = kbRun(t, kbBin, k, "", "chat", "read", "Bt_"+rnd)
		require.Contains(t, out, "from the bot")
		kbRun(t, kbBin, k, "", "logout")
		out, errOut, code = kbRunEnv(t, kbBin, k, []string{"KB_BOT_TOKEN=" + tok.Token}, "", "login", "--bot-token", "--server", tok.Host)
		require.Equal(t, 0, code, "%s%s", out, errOut)
		require.Contains(t, out, "Signed in as "+alice+"@")
		out = kbRun(t, kbBin, k, "", "device", "ls")
		require.Contains(t, out, tok.Name+"\tbot token\t")
		require.Contains(t, out, "this device")
		require.Empty(t, filesHolding(t, home4, secret), "kb or chatd wrote the token down")
		k.stop()

		// Revoke by name, as for any key.
		out = kbRun(t, kbBin, a, "", "device", "revoke", tok.Name)
		t.Logf("kb device revoke:\n%s", out)
		require.Contains(t, out, "Revoked "+tok.Name+" (bot token)")
	} else {
		a.ok("revoke_device", map[string]any{"id": botKey.ID}, nil)
	}
	merklePoke(t)
	merklePoke(t)
	for _, d := range listDevices(a) {
		if d.ID == botKey.ID {
			require.True(t, d.Revoked, "%+v", d)
		}
	}

	// A running bot keeps its session until it reconnects (as any revoked
	// device); once it signs in again the server refuses the token.
	b.stop()
	b = startChatdAt(t, cfg2.cfgFlags, home2)
	f = b.call("login_bot_token", map[string]any{"host": tok.Host, "token": tok.Token})
	require.False(t, f.OK, "a revoked token signed in")
	t.Logf("the revoked token: %+v", f.Error)
	require.Equal(t, "auth", f.Error.Code, "%+v", f.Error)
	require.Contains(t, f.Error.Message, "revoked")
	g := b.call("get_thread_view", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "before": 0, "num": 50})
	require.False(t, g.OK, "the revoked bot reads")
	g = b.call("send", map[string]any{"team": team, "app": 1, "channel": defaultChannel(), "body": "from the revoked bot"})
	require.False(t, g.OK, "the revoked bot sends")
	t.Logf("the revoked bot sends: %+v", g.Error)

	// Its account row is still on the bot's home; remove_account clears it.
	require.Contains(t, b.localUsers(), alice)
	var removed wireRemoved
	b.ok("remove_account", map[string]any{"user": alice}, &removed)
	t.Logf("removed from the bot's home: %+v", removed)
	require.Equal(t, alice, removed.Username)
	require.False(t, removed.DeviceKey)
	require.Empty(t, b.localUsers())
	require.Nil(t, b.status().User)

	// alice's own device is untouched.
	sendText(a, team, "after the revoke")
	require.Len(t, threadMsgs(a, team, defaultChannel()), 3)
}

// TestChatdV3RemoveAccount: remove_account takes an account's keys and
// entry off one device (a device key included, which `foks key remove`
// refuses), leaves a second account on that device alone, and changes
// nothing on the server: the other device goes on working, and the
// removed device's key stays in the key list until it is revoked.
func TestChatdV3RemoveAccount(t *testing.T) {
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	cfg := newTestAgent(t)
	a := startChatdAt(t, cfg.cfgFlags, t.TempDir())
	a.ok("subscribe", map[string]any{"app": 1}, nil)
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice, bob := "alice_"+rnd, "bob_"+rnd
	a.signupOverWire(vh.Addr, alice, "desk")
	a.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	var created struct {
		Team map[string]any `json:"team"`
	}
	a.ok("create_team", map[string]any{"name": "Rm_" + rnd}, &created)
	team := created.Team
	merklePoke(t)
	sendText(a, team, "one")
	merklePoke(t)
	var bkp struct {
		Key string `json:"key"`
	}
	a.ok("create_backup_key", nil, &bkp)
	merklePoke(t)

	// The laptop: alice by her backup key, then a second account, bob.
	cfg2 := newTestAgent(t)
	home2 := t.TempDir()
	d := startChatdAt(t, cfg2.cfgFlags, home2)
	d.ok("subscribe", map[string]any{"app": 1}, nil)
	var login struct {
		User *wireUserStatus `json:"user"`
	}
	d.ok("login_backup", map[string]any{"key": bkp.Key, "device": "laptop", "server": string(vh.Addr)}, &login)
	require.Equal(t, alice, login.User.Username)
	d.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	require.Len(t, threadMsgs(d, team, defaultChannel()), 1)
	d.ok("logout", nil, nil)
	d.signupOverWire(vh.Addr, bob, "laptop")
	merklePoke(t)
	merklePoke(t)
	require.Equal(t, map[string]bool{alice: false, bob: true}, d.localUsers())

	// alice, who is not the active one: gone from the list; bob is not
	// signed out and nothing is announced.
	d.drainEvents()
	f := d.call("remove_account", map[string]any{"user": "nobody_" + rnd})
	require.Equal(t, "bad_args", f.Error.Code, "%+v", f.Error)
	var removed wireRemoved
	d.ok("remove_account", map[string]any{"user": alice, "host": d.hostOf(alice)}, &removed)
	t.Logf("removed: %+v", removed)
	require.Equal(t, alice, removed.Username)
	require.False(t, removed.WasActive)
	require.True(t, removed.DeviceKey)
	require.Equal(t, map[string]bool{bob: true}, d.localUsers())
	require.Equal(t, bob, d.status().User.Username)
	select {
	case ev := <-d.events:
		require.NotEqual(t, "user_changed", ev.Event, "removing another account announced a change")
	case <-time.After(500 * time.Millisecond):
	}
	d.ok("list_teams", nil, nil)
	f = d.call("login", map[string]any{"username": alice})
	require.Equal(t, "bad_args", f.Error.Code, "the removed account signs in: %+v", f.Error)
	require.Equal(t, bob, d.status().User.Username)

	// bob, the active one, by default: signed out and announced.
	d.ok("remove_account", nil, &removed)
	require.Equal(t, bob, removed.Username)
	require.True(t, removed.WasActive)
	require.True(t, removed.DeviceKey)
	d.event("user_changed", 10*time.Second)
	require.Nil(t, d.status().User)
	require.Empty(t, d.localUsers())
	f = d.call("remove_account", nil)
	require.Equal(t, "rejected", f.Error.Code, "%+v", f.Error)
	f = d.call("list_teams", nil)
	require.False(t, f.OK)

	// It is not a matter of chatd's memory: the same after a restart.
	d.stop()
	d = startChatdAt(t, cfg2.cfgFlags, home2)
	require.Nil(t, d.status().User)
	require.Empty(t, d.localUsers())

	// Nothing changed on the server. The desk works; the laptop's key is
	// still in alice's list, not revoked, until the desk revokes it.
	sendText(a, team, "two")
	merklePoke(t)
	require.Len(t, threadMsgs(a, team, defaultChannel()), 2)
	var laptop wireDevice
	for _, dev := range listDevices(a) {
		if dev.Name == "laptop" {
			laptop = dev
		}
	}
	require.NotEmpty(t, laptop.ID, "the laptop's key left the account's list")
	require.False(t, laptop.Revoked)
	a.ok("revoke_device", map[string]any{"id": laptop.ID}, nil)
	merklePoke(t)

	// And the account can come back to that device with the backup key.
	d.ok("login_backup", map[string]any{"key": bkp.Key, "device": "laptop again", "server": string(vh.Addr)}, &login)
	require.Equal(t, alice, login.User.Username)
	require.Equal(t, "laptop again", login.User.Device)
	merklePoke(t)
	require.Len(t, threadMsgs(d, team, defaultChannel()), 2)
}

// TestKbCLIForget: `kb logout --forget` against a real chatd. It refuses to
// lock an account out, asks for the username, names what can still sign
// in, and afterwards the account is gone from that chatd while the other
// device works.
func TestKbCLIForget(t *testing.T) {
	bin := os.Getenv("KB_BIN")
	if bin == "" {
		t.Skip("set KB_BIN to the kb CLI binary to run this test")
	}
	stopper := runMerkleActivePoker(t)
	defer stopper()
	vh := vHost(t, 4)

	cfg := newTestAgent(t)
	a := startChatdAt(t, cfg.cfgFlags, t.TempDir())
	rnd := strings.ToLower(fsRandomString(t, 6))
	alice := "alice_" + rnd
	a.signupOverWire(vh.Addr, alice, "desk")
	merklePoke(t)
	merklePoke(t)
	tm := "Fg_" + rnd
	kbRun(t, bin, a, "", "team", "create", tm)
	merklePoke(t)
	kbRun(t, bin, a, "", "chat", "send", tm, "one")
	merklePoke(t)

	// The only device and no backup key: refused, with what to do.
	out, errOut, code := kbRunCode(t, bin, a, "", "logout", "--forget", "--yes")
	require.Equal(t, 1, code, "%s%s", out, errOut)
	require.Contains(t, errOut, "lock you out of the account for good")
	require.Contains(t, errOut, "kb backup create")
	require.Equal(t, map[string]bool{alice: true}, a.localUsers())

	var bkp struct {
		Key string `json:"key"`
	}
	a.ok("create_backup_key", nil, &bkp)
	merklePoke(t)
	backupName := strings.Join(strings.Fields(bkp.Key)[:2], " ")

	// A second device.
	cfg2 := newTestAgent(t)
	d := startChatdAt(t, cfg2.cfgFlags, t.TempDir())
	out = kbRun(t, bin, d, bkp.Key+"\n", "login", "--backup", "--passphrase-stdin", "--device", "laptop", "--server", string(vh.Addr))
	require.Contains(t, out, "Signed in as "+alice+"@")
	merklePoke(t)
	merklePoke(t)

	// Not confirmed: nothing happens.
	_, errOut, code = kbRunCode(t, bin, d, "someone_else\n", "logout", "--forget")
	require.Equal(t, 2, code, "%s", errOut)
	t.Logf("kb logout --forget asks:\n%s", errOut)
	require.Contains(t, errOut, "Remove "+alice+"@")
	require.Contains(t, errOut, "The account stays on the server")
	require.Contains(t, errOut, "Still able to sign in:")
	require.Contains(t, errOut, "desk (computer)")
	require.Contains(t, errOut, backupName+" (backup key)")
	require.Contains(t, errOut, "nothing was removed")
	require.Equal(t, map[string]bool{alice: true}, d.localUsers())

	// The username: removed.
	out, errOut, code = kbRunCode(t, bin, d, alice+"\n", "logout", "--forget")
	require.Equal(t, 0, code, "%s%s", out, errOut)
	t.Logf("kb logout --forget:\n%s", out)
	require.Contains(t, out, "Removed "+alice+"@")
	require.Contains(t, out, "still on the server")
	require.Contains(t, out, "kb device revoke laptop")
	require.Empty(t, d.localUsers())
	require.Nil(t, d.status().User)
	out, _, code = kbRunCode(t, bin, d, "", "status")
	require.Equal(t, 5, code, "kb status after --forget: %s", out)
	require.Contains(t, out, "run `kb signup`")
	_, _, code = kbRunCode(t, bin, d, "", "login", alice)
	require.Equal(t, 1, code, "the forgotten account signs in")

	// The desk still works, and takes the laptop's key off the account.
	kbRun(t, bin, a, "", "chat", "send", tm, "two")
	merklePoke(t)
	out = kbRun(t, bin, a, "", "chat", "read", tm, "--peek")
	require.Contains(t, out, "one")
	require.Contains(t, out, "two")
	out = kbRun(t, bin, a, "", "device", "revoke", "laptop")
	require.Contains(t, out, "Revoked laptop")

	// --yes --force on an account with nothing else: gone, as asked.
	cfg3 := newTestAgent(t)
	e := startChatdAt(t, cfg3.cfgFlags, t.TempDir())
	bob := "bob_" + rnd
	e.signupOverWire(vh.Addr, bob, "only")
	merklePoke(t)
	out, errOut, code = kbRunCode(t, bin, e, "", "logout", "--forget", "--yes", "--force")
	require.Equal(t, 0, code, "%s%s", out, errOut)
	require.Contains(t, out, "Removed "+bob+"@")
	require.Contains(t, out, "Nothing else can sign in")
	require.Empty(t, e.localUsers())
}
