//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo. It
// drives `kb git` and the real git binary (through kb as git-remote-foks,
// WIRE.md "Git") against real chatds on the test server: a team's
// repository cloned, pushed to and pulled from by two members, a user's
// own repository, and a non-member who is refused. Build chatd and kb
// first, then:
//
//	CHATD_TEST_BIN=<chatd.exe> KB_BIN=<kb.exe> go test -tags chatfoks \
//	    ./integration-tests/cli -run TestKbCLIGit

package cli

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKbCLIGit(t *testing.T) {
	kbBin := os.Getenv("KB_BIN")
	if kbBin == "" {
		t.Skip("KB_BIN is not set")
	}
	gitBin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	r := newAttRig(t)
	defer r.close()

	kb := func(c *chatdClient, args ...string) string {
		t.Helper()
		return kbRun(t, kbBin, c, "", append([]string{"git"}, args...)...)
	}
	kbFails := func(c *chatdClient, code int, says string, args ...string) {
		t.Helper()
		out, errOut, got := kbRunCode(t, kbBin, c, "", append([]string{"git"}, args...)...)
		require.Equal(t, code, got, "kb git %v: stdout %q stderr %q", args, out, errOut)
		require.Contains(t, errOut, says, "kb git %v", args)
		require.Empty(t, out, "kb git %v printed before failing", args)
	}

	// `kb git setup` makes the helper git looks for: kb under the name
	// git-remote-foks, in a folder this test puts first on PATH.
	helperDir := t.TempDir()
	out := kb(r.alice, "setup", "--dir", helperDir)
	t.Logf("kb git setup: %s", strings.TrimSpace(out))
	helper := "git-remote-foks"
	if strings.HasSuffix(strings.ToLower(kbBin), ".exe") {
		helper += ".exe"
	}
	_, err = os.Stat(filepath.Join(helperDir, helper))
	require.NoError(t, err, "kb git setup made no helper")

	// git with nobody's own configuration, and the helper pointed at one
	// user's chatd.
	gitHome := t.TempDir()
	emptyCfg := filepath.Join(gitHome, "gitconfig")
	require.NoError(t, os.WriteFile(emptyCfg, nil, 0o600))
	git := func(c *chatdClient, who, dir string, args ...string) (string, int) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		full := append([]string{
			"-c", "user.name=" + who, "-c", "user.email=" + who + "@example.invalid",
			"-c", "init.defaultBranch=main", "-c", "core.autocrlf=false", "-c", "gc.auto=0",
		}, args...)
		cmd := exec.CommandContext(ctx, gitBin, full...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"PATH="+helperDir+string(os.PathListSeparator)+os.Getenv("PATH"),
			"KB_CHATD_SOCKET=tcp://"+c.addr,
			"KB_CHATD_TOKEN_FILE="+c.tokenFile,
			"GIT_CONFIG_GLOBAL="+emptyCfg,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_TERMINAL_PROMPT=0",
			"HOME="+gitHome,
		)
		b, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			exitErr, ok := err.(*exec.ExitError)
			require.True(t, ok, "git %v: %v\n%s", args, err, b)
			code = exitErr.ExitCode()
		}
		t.Logf("%s: git %s -> exit %d\n%s", who, strings.Join(args, " "), code, strings.TrimSpace(string(b)))
		return string(b), code
	}
	gitOK := func(c *chatdClient, who, dir string, args ...string) string {
		t.Helper()
		out, code := git(c, who, dir, args...)
		require.Equal(t, 0, code, "git %v failed:\n%s", args, out)
		return out
	}
	commit := func(c *chatdClient, who, dir, file, content, msg string) {
		t.Helper()
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644))
		gitOK(c, who, dir, "add", file)
		gitOK(c, who, dir, "commit", "-q", "-m", msg)
	}
	read := func(dir, file string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, file))
		require.NoError(t, err)
		return string(b)
	}

	type repo struct {
		Name string  `json:"name"`
		Team *string `json:"team"`
		URL  string  `json:"url"`
	}
	list := func(c *chatdClient, args ...string) []repo {
		t.Helper()
		var repos []repo
		require.NoError(t, json.Unmarshal([]byte(kb(c, append([]string{"ls", "--json"}, args...)...)), &repos))
		return repos
	}

	// --- a team's repository ---

	var created struct {
		Team map[string]any `json:"team"`
	}
	r.alice.ok("create_team", map[string]any{"name": "Git_" + r.rnd}, &created)
	team := created.Team["value"].(string)
	merklePoke(t)
	r.alice.ok("team_add_member", map[string]any{"team": created.Team, "users": []string{r.bobNm}}, nil)
	merklePoke(t)

	require.Empty(t, list(r.alice), "nobody has made a repository yet")
	// A team's first repository makes folders at the top of the team's
	// store, which only an admin may.
	kbFails(r.bob, 1, "auth: You can't change bobs", "create", "bobs", "--team", team)
	out = kb(r.alice, "create", "Site", "--team", team)
	t.Logf("kb git create:\n%s", out)
	repos := list(r.alice, "--team", team)
	require.Len(t, repos, 1)
	require.Equal(t, "site", repos[0].Name)
	require.NotNil(t, repos[0].Team)
	url := repos[0].URL
	require.Regexp(t, `^foks://[^/]+/t:[^/]+/site$`, url)
	require.Contains(t, out, "Created "+url+"\n")
	t.Logf("kb git ls:\n%s", kb(r.alice, "ls"))
	kbFails(r.alice, 1, "already a repository named site", "create", "site", "--team", team)
	// Once the team has one, every member can make and delete them.
	kb(r.bob, "create", "bobs", "--team", team)
	require.Len(t, list(r.alice, "--team", team), 2)
	require.Equal(t, "Deleted bobs\n", kb(r.bob, "delete", "bobs", "--team", team, "--yes"))
	require.Len(t, list(r.bob, "--team", team), 1)

	// Alice clones it (empty), commits and pushes.
	aliceDir := filepath.Join(t.TempDir(), "site")
	start := time.Now()
	gitOK(r.alice, "alice", filepath.Dir(aliceDir), "clone", url, aliceDir)
	t.Logf("alice: clone of the empty repository took %v", time.Since(start))
	commit(r.alice, "alice", aliceDir, "README.md", "hello from alice\n", "first")
	gitOK(r.alice, "alice", aliceDir, "branch", "-M", "main")
	start = time.Now()
	gitOK(r.alice, "alice", aliceDir, "push", "-u", "origin", "main")
	t.Logf("alice: first push took %v", time.Since(start))

	// Bob, on his own chatd, clones it and sees the commit; he pushes a
	// second one.
	bobDir := filepath.Join(t.TempDir(), "site")
	gitOK(r.bob, "bob", filepath.Dir(bobDir), "clone", url, bobDir)
	require.Equal(t, "hello from alice\n", read(bobDir, "README.md"))
	log := gitOK(r.bob, "bob", bobDir, "log", "--format=%an: %s")
	require.Equal(t, "alice: first\n", log)
	commit(r.bob, "bob", bobDir, "notes.txt", "hello from bob\n", "second")
	gitOK(r.bob, "bob", bobDir, "push", "origin", "main")

	// Alice pulls it.
	gitOK(r.alice, "alice", aliceDir, "pull", "--ff-only", "origin", "main")
	require.Equal(t, "hello from bob\n", read(aliceDir, "notes.txt"))
	log = gitOK(r.alice, "alice", aliceDir, "log", "--format=%an: %s")
	require.Equal(t, "bob: second\nalice: first\n", log)

	// A push that is not a fast-forward is refused, as git does.
	commit(r.alice, "alice", aliceDir, "a.txt", "a\n", "third, from alice")
	gitOK(r.alice, "alice", aliceDir, "push", "origin", "main")
	commit(r.bob, "bob", bobDir, "b.txt", "b\n", "third, from bob")
	stale, code := git(r.bob, "bob", bobDir, "push", "origin", "main")
	require.NotEqual(t, 0, code, "a push over someone else's commit went through:\n%s", stale)
	require.Contains(t, stale, "[rejected]")
	require.Contains(t, stale, "non-fast-forward")
	gitOK(r.bob, "bob", bobDir, "pull", "--rebase", "origin", "main")
	gitOK(r.bob, "bob", bobDir, "push", "origin", "main")
	gitOK(r.alice, "alice", aliceDir, "pull", "--ff-only", "origin", "main")
	require.Equal(t, "b\n", read(aliceDir, "b.txt"))

	// A second branch, and fetching it.
	gitOK(r.bob, "bob", bobDir, "checkout", "-q", "-b", "topic")
	commit(r.bob, "bob", bobDir, "topic.txt", "topic\n", "on a branch")
	gitOK(r.bob, "bob", bobDir, "push", "origin", "topic")
	gitOK(r.alice, "alice", aliceDir, "fetch", "origin")
	branches := gitOK(r.alice, "alice", aliceDir, "branch", "-r", "--format=%(refname:short)")
	require.Contains(t, branches, "origin/topic")
	require.Contains(t, branches, "origin/main")

	// --- someone who is not in the team ---

	vh := vHost(t, 4)
	daveCfg := newTestAgent(t)
	dave := startChatdFresh(t, daveCfg.cfgFlags)
	dave.ok("subscribe", map[string]any{"app": 1}, nil)
	dave.signupOverWire(vh.Addr, "dave_"+r.rnd, "dave git")
	dave.event("user_changed", 10*time.Second)
	merklePoke(t)
	merklePoke(t)
	daveDir := filepath.Join(t.TempDir(), "site")
	refused, code := git(dave, "dave", filepath.Dir(daveDir), "clone", url, daveDir)
	require.NotEqual(t, 0, code, "someone outside the team cloned it:\n%s", refused)
	require.Contains(t, refused, "kb: auth: You can't use "+url)
	require.Contains(t, refused, "you aren't in this team")
	_, err = os.Stat(filepath.Join(daveDir, "README.md"))
	require.True(t, os.IsNotExist(err), "a refused clone left files")
	kbFails(dave, 1, "auth:", "ls", "--team", team)
	kbFails(dave, 1, "auth:", "delete", "site", "--team", team, "--yes")
	require.Empty(t, list(dave), "dave has no repositories")

	// --- a user's own repository ---

	out = kb(r.alice, "create", "notes")
	own := list(r.alice)
	require.Len(t, own, 2, "alice's own, then her team's")
	require.Equal(t, "notes", own[0].Name)
	require.Nil(t, own[0].Team)
	require.Equal(t, "site", own[1].Name)
	ownURL := own[0].URL
	require.Regexp(t, `^foks://[^/]+/alice_[^/]+/notes$`, ownURL)
	require.Contains(t, out, "Created "+ownURL+"\n")
	// Bob sees the team's repository in his list, and not alice's own.
	bobs := list(r.bob)
	require.Len(t, bobs, 1)
	require.Equal(t, url, bobs[0].URL)

	// An existing checkout gets it as a second remote.
	gitOK(r.alice, "alice", aliceDir, "remote", "add", "mine", ownURL)
	gitOK(r.alice, "alice", aliceDir, "push", "mine", "main")
	ownDir := filepath.Join(t.TempDir(), "notes")
	gitOK(r.alice, "alice", filepath.Dir(ownDir), "clone", ownURL, ownDir)
	require.Equal(t, "hello from alice\n", read(ownDir, "README.md"))
	// Nobody else can use it.
	bobOwn := filepath.Join(t.TempDir(), "notes")
	refused, code = git(r.bob, "bob", filepath.Dir(bobOwn), "clone", ownURL, bobOwn)
	require.NotEqual(t, 0, code, "bob cloned alice's own repository:\n%s", refused)
	require.Contains(t, refused, "is someone's own repository")

	// An address of no repository.
	noDir := filepath.Join(t.TempDir(), "nope")
	refused, code = git(r.alice, "alice", filepath.Dir(noDir), "clone", strings.TrimSuffix(url, "site")+"nope", noDir)
	require.NotEqual(t, 0, code)
	require.Contains(t, refused, "row_not_found: There is no repository")

	// --- delete ---

	kbFails(r.alice, 2, "--yes", "delete", "site", "--team", team)
	require.Equal(t, "Deleted site\n", kb(r.alice, "delete", "site", "--team", team, "--yes"))
	kbFails(r.alice, 1, "row_not_found", "delete", "site", "--team", team, "--yes")
	require.Empty(t, list(r.alice, "--team", team))
	gone, code := git(r.bob, "bob", bobDir, "fetch", "origin")
	require.NotEqual(t, 0, code, "a deleted repository still answers:\n%s", gone)
	require.Contains(t, gone, "There is no repository")
	require.Len(t, list(r.alice), 1, "alice's own is still there")
}
