//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// This file lives only in the ephb/go-foks fork, for the chat-foks repo
// (github.com/ephb/chat-foks). It imports backend/shim, a separate Go
// module that resolves only through chat-foks's root go.work, so it is
// behind the "chatfoks" build tag and the fork still builds on its own.
// Run it from a chat-foks checkout:
//
//	go test -tags chatfoks ./integration-tests/lib -run 'Shim|Chatd'
//
// It drives the kb-core <-> FOKS adapter (backend/shim/CONTRACT.md) against
// the same Docker-backed rig every other outbox test here uses.

// TestShimAdapterRoundTrip exercises every Shim operation that does not
// require simulating a transport failure (the outbox/idempotency semantics
// those need are already proven at the Minder level by
// TestRTSendIdempotentReplay/TestRTOutboxQueueAndDrain/TestRTOutboxLostAck/
// TestRTOutboxNoQueueJumping in this same file -- this test's job is the
// adapter's translation layer, not re-proving FOKS's own semantics).
package lib

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"

	"github.com/terra/kebase-fork/backend/shim"
	"github.com/terra/kebase-fork/backend/shim/wire"
)
func TestShimAdapterRoundTrip(t *testing.T) {
	r := makeRTOutboxTestRig(t)
	ctx := r.mb.Ctx()
	g := r.mb.Base().G()

	sh, err := shim.NewAdapter(g)
	require.NoError(t, err)

	teamRef := team.WrapNamedPtr(r.fqt)
	ch := shim.Channel{
		Team:    teamRef,
		AppID:   proto.RTAppID_Chat,
		Channel: makeChannelSpecifierWithString("foo"),
	}

	// Send through the adapter's own Minder instance (independent of the
	// rig's r.minder, no test hooks installed on it) and confirm a real Ack.
	res, err := sh.Send(ctx, ch, []byte("hello from the adapter"))
	require.NoError(t, err)
	require.NotNil(t, res.Ack)
	require.Nil(t, res.Queued)
	require.False(t, res.Ack.WasReplay)
	require.Equal(t, proto.RTMsgSeq(1), res.Ack.Seq)

	// Sending the exact same body is a distinct message (a fresh msgID is
	// minted per Send call) -- confirm no accidental replay collapsing on
	// the adapter's side.
	res2, err := sh.Send(ctx, ch, []byte("hello from the adapter"))
	require.NoError(t, err)
	require.NotNil(t, res2.Ack)
	require.False(t, res2.Ack.WasReplay)
	require.Equal(t, proto.RTMsgSeq(2), res2.Ack.Seq)

	// Read it back through GetThreadView: msgs present, no pending overlay
	// (both sends are already acked).
	view, err := sh.GetThreadView(ctx, ch, 0, 10)
	require.NoError(t, err)
	require.Len(t, view.Msgs, 2)
	require.Equal(t, "hello from the adapter", string(view.Msgs[0].Body))
	require.Len(t, view.Pending, 0)

	// GetThreadPage mirrors the same data through the lower-level shape.
	page, err := sh.GetThreadPage(ctx, ch, 0, 10)
	require.NoError(t, err)
	require.Len(t, page.Msgs, 2)
	require.False(t, page.Stale)
	require.NoError(t, page.TransportErr)

	// ListChannelsForTeam sees the rig's "foo" channel.
	set, err := sh.ListChannelsForTeam(ctx, teamRef, proto.RTAppID_Chat)
	require.NoError(t, err)
	found := false
	for _, c := range set.Channels {
		if string(c.Name) == "foo" {
			found = true
		}
	}
	require.True(t, found, "expected to find the rig's foo channel, got %+v", set.Channels)

	// Drain with nothing queued is a clean no-op.
	dres, err := sh.Drain(ctx)
	require.NoError(t, err)
	require.Empty(t, dres.Acked)
	require.Empty(t, dres.Failed)
	require.NoError(t, dres.TransportErr)

	// ListOutbox is empty: both sends above went straight through.
	rows, err := sh.ListOutbox(ctx)
	require.NoError(t, err)
	require.Empty(t, rows)

	// ReadThrough marking the latest seq read is fire-and-forget (nil error
	// does not distinguish "acked" from "queued for replay" by design).
	require.NoError(t, sh.ReadThrough(ctx, ch, proto.RTMsgSeq(2)))

	// --- Error classification round trip, through the real adapter ---

	// DiscardOutbox on a msgID that was never queued classifies as
	// ErrRowNotFound.
	var bogus proto.RTMsgID
	err = sh.DiscardOutbox(ctx, bogus)
	require.Error(t, err)
	code, ok := shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrRowNotFound, code)

	// RetryOutbox on the same: also ErrRowNotFound (distinct code path in
	// the adapter/Minder from DiscardOutbox, same classification).
	_, err = sh.RetryOutbox(ctx, bogus)
	require.Error(t, err)
	code, ok = shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrRowNotFound, code)

	// MakeChannel with a name that already exists at the same (default)
	// tier classifies as ErrChannelExists.
	_, err = sh.MakeChannel(ctx, teamRef, proto.RTAppID_Chat,
		"foo", "a duplicate of the rig's channel", proto.RolePairOpt{})
	require.Error(t, err)
	code, ok = shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrChannelExists, code)

	// UpdateChannel: non-empty-name update on the rig's channel succeeds.
	err = sh.UpdateChannel(ctx, ch, shim.ChannelUpdate{
		Desc: func() *proto.RTChannelDesc { d := proto.RTChannelDesc("updated by the adapter"); return &d }(),
	})
	require.NoError(t, err)

	// UpdateChannel with neither Name nor Desc set is a real, genuine
	// core.BadArgsError ("channel update changes nothing",
	// client/librt/minder.go) -- unlike the two cases below, which looked
	// like bad_args by their error *message* but are actually
	// core.RTGenericError (caught by running this test for real: an
	// earlier draft of this file asserted ErrBadArgs for both and was
	// wrong, per the "actual: 0 (ErrUnknown)" failure this replaced).
	err = sh.UpdateChannel(ctx, ch, shim.ChannelUpdate{})
	require.Error(t, err)
	code, ok = shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrBadArgs, code)

	// SetChannelArchived on the team's default (nameless) channel is
	// refused client-side -- but as a plain core.RTGenericError
	// ("cannot archive a team's default channel"), not core.BadArgsError,
	// so Classify correctly reports ErrUnknown/"other" here, not
	// ErrBadArgs. Must run before the ambiguous-channel sub-test below:
	// once a second empty-name channel exists at another tier, the
	// "default channel" None-specifier itself becomes ambiguous too (same
	// resolution path as an explicit Name{"", no tier} lookup), which
	// would fail this assertion for an unrelated reason.
	defaultCh := shim.Channel{
		Team:    teamRef,
		AppID:   proto.RTAppID_Chat,
		Channel: lcl.NewRTChannelSpecifierDefault(lcl.RTChannelSpecifierType_None),
	}
	err = sh.SetChannelArchived(ctx, defaultCh, true)
	require.Error(t, err)
	code, ok = shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrUnknown, code)

	// Ambiguous channel resolution. The non-empty-name collision check above
	// is team-wide regardless of tier (MakeChannelOpts.AllowDuplicateName's
	// doc, client/librt/minder.go: "skips the team-wide name-collision
	// check"), so two channels cannot share a non-empty name at different
	// tiers at all -- that path can never be ambiguous. The EMPTY name is
	// the one deliberate exception ("Never for the EMPTY name, which stays
	// unique [per tier] so that None-specifier lookups of the default
	// channel resolve"): a fresh team may have one empty-name channel per
	// tier, and resolving "" with no tier given is exactly what's ambiguous
	// between them. This mirrors TestRTChannelDisambiguation in rt_test.go
	// exactly (same two-tier, empty-name construction on a fresh team), and
	// is also why the None-specifier default-channel assertion above had to
	// run first: creating these makes "the default channel" ambiguous too.
	_, err = sh.MakeChannel(ctx, teamRef, proto.RTAppID_Chat,
		"", "admin-tier default channel", proto.RolePairOpt{Read: &proto.AdminRole})
	require.NoError(t, err, "setup for the ambiguous-channel case failed")
	_, err = sh.MakeChannel(ctx, teamRef, proto.RTAppID_Chat,
		"", "bottom-tier default channel", proto.RolePairOpt{Read: &proto.DefaultRole})
	require.NoError(t, err, "setup for the ambiguous-channel case failed")

	_, err = sh.GetThreadView(ctx, shim.Channel{
		Team:    teamRef,
		AppID:   proto.RTAppID_Chat,
		Channel: makeChannelSpecifierWithString(""),
	}, 0, 10)
	require.Error(t, err)
	code, ok = shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrAmbiguousChannel, code)
}

func fqTeamToWireString(t *testing.T, fqt *proto.FQTeamParsed) string {
	tmStr, err := fqt.Team.StringErr()
	require.NoError(t, err)
	if fqt.Host == nil {
		return tmStr
	}
	hostStr, err := fqt.Host.StringErr()
	require.NoError(t, err)
	return fmt.Sprintf("%s@%s", tmStr, hostStr)
}

// TestChatdWireSocketRoundTrip spins up wire.Server over a real Unix-domain
// socket (on Windows and Posix alike) backed by shim.NewAdapter on the Docker
// test rig, and exercises the WIRE.md length-prefixed JSON protocol end to end.
func TestChatdWireSocketRoundTrip(t *testing.T) {
	r := makeRTOutboxTestRig(t)
	g := r.mb.Base().G()

	sh, err := shim.NewAdapter(g)
	require.NoError(t, err)

	const token = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
	srv := wire.NewServer(sh, nil, token, "chatd/test")
	sockPath := filepath.Join(t.TempDir(), "chatd.sock")
	l, err := net.Listen("unix", sockPath)
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(r.mb.Ctx())
	t.Cleanup(func() {
		cancel()
		_ = l.Close()
	})
	go func() {
		_ = srv.Serve(ctx, l)
	}()

	conn, err := net.Dial("unix", sockPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	var nextID int64
	call := func(op string, params any) wire.Response {
		nextID++
		var rawParams json.RawMessage
		if params != nil {
			b, err := json.Marshal(params)
			require.NoError(t, err)
			rawParams = b
		}
		reqBytes, err := json.Marshal(wire.Request{
			V:      3,
			ID:     nextID,
			Op:     op,
			Params: rawParams,
		})
		require.NoError(t, err)
		require.NoError(t, wire.WriteFrame(conn, reqBytes))

		respBytes, err := wire.ReadFrame(conn)
		require.NoError(t, err)
		var resp wire.Response
		require.NoError(t, json.Unmarshal(respBytes, &resp))
		require.Equal(t, 3, resp.V)
		require.Equal(t, nextID, resp.ID)
		return resp
	}

	// 0. hello must come first (WIRE.md "Authentication").
	helloResp := call("hello", map[string]any{"token": token, "client": "shim-adapter-test/0"})
	require.True(t, helloResp.OK, "hello failed: %+v", helloResp.Error)

	teamStr := fqTeamToWireString(t, r.fqt)
	teamParam := map[string]any{"kind": "named", "value": teamStr}
	fooChParam := map[string]any{"kind": "name", "name": "foo"}

	// 1. send -> outcome: "acked", seq: 1, was_replay: false
	sendResp := call("send", map[string]any{
		"team":    teamParam,
		"app":     1,
		"channel": fooChParam,
		"body":    "hello over wire",
	})
	require.True(t, sendResp.OK, "send failed: %+v", sendResp.Error)
	var sendRes struct {
		Outcome    string `json:"outcome"`
		Seq        uint64 `json:"seq"`
		InsertTime uint64 `json:"insert_time"`
		WasReplay  bool   `json:"was_replay"`
	}
	require.NoError(t, json.Unmarshal(sendResp.Result, &sendRes))
	require.Equal(t, "acked", sendRes.Outcome)
	require.Equal(t, uint64(1), sendRes.Seq)
	require.False(t, sendRes.WasReplay)
	require.NotZero(t, sendRes.InsertTime)

	// 2. get_thread_view -> messages has sender (PartyID string), sender_name, prev_id: null, prev_seq: null
	tvResp := call("get_thread_view", map[string]any{
		"team":    teamParam,
		"app":     1,
		"channel": fooChParam,
		"before":  0,
		"num":     10,
	})
	require.True(t, tvResp.OK, "get_thread_view failed: %+v", tvResp.Error)
	var tvRes struct {
		Messages []struct {
			MsgID      string  `json:"msg_id"`
			Seq        uint64  `json:"seq"`
			PrevID     *string `json:"prev_id"`
			PrevSeq    *uint64 `json:"prev_seq"`
			Typ        string  `json:"typ"`
			Sender     string  `json:"sender"`
			SenderName *string `json:"sender_name"`
			SentAt     uint64  `json:"sent_at"`
			InsertTime uint64  `json:"insert_time"`
			Body       string  `json:"body"`
		} `json:"messages"`
		Pending     []any `json:"pending"`
		AtBeginning bool  `json:"at_beginning"`
		Stale       bool  `json:"stale"`
	}
	require.NoError(t, json.Unmarshal(tvResp.Result, &tvRes))
	require.Len(t, tvRes.Messages, 1)
	msg0 := tvRes.Messages[0]
	require.Equal(t, uint64(1), msg0.Seq)
	require.Nil(t, msg0.PrevID)
	require.Nil(t, msg0.PrevSeq)
	require.Equal(t, "basic", msg0.Typ)
	require.NotEmpty(t, msg0.Sender)
	require.NotEqual(t, ".", msg0.Sender)
	require.Equal(t, "hello over wire", msg0.Body)
	require.Empty(t, tvRes.Pending)
	require.False(t, tvRes.Stale)

	// 3. send_typed (reply to msg0.MsgID) -> outcome: "acked", seq: 2
	replyResp := call("send_typed", map[string]any{
		"team":     teamParam,
		"app":      1,
		"channel":  fooChParam,
		"body":     "reply over wire",
		"typ":      "reply",
		"reply_to": msg0.MsgID,
	})
	require.True(t, replyResp.OK, "send_typed failed: %+v", replyResp.Error)
	var replyRes struct {
		Outcome string `json:"outcome"`
		Seq     uint64 `json:"seq"`
	}
	require.NoError(t, json.Unmarshal(replyResp.Result, &replyRes))
	require.Equal(t, "acked", replyRes.Outcome)
	require.Equal(t, uint64(2), replyRes.Seq)

	// 4. list_channels_for_team -> echoes request's team object verbatim, includes "foo"
	lcResp := call("list_channels_for_team", map[string]any{
		"team": teamParam,
		"app":  1,
	})
	require.True(t, lcResp.OK, "list_channels_for_team failed: %+v", lcResp.Error)
	var lcRes struct {
		Team struct {
			Kind  string `json:"kind"`
			Value string `json:"value"`
		} `json:"team"`
		App      int `json:"app"`
		Channels []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Tier string `json:"tier"`
		} `json:"channels"`
	}
	require.NoError(t, json.Unmarshal(lcResp.Result, &lcRes))
	require.Equal(t, "named", lcRes.Team.Kind)
	require.Equal(t, teamStr, lcRes.Team.Value)
	require.Equal(t, 1, lcRes.App)
	require.NotEmpty(t, lcRes.Channels)

	// 5. make_channel -> returns {"id": "rtch1..."}; duplicate -> channel_exists
	mcResp := call("make_channel", map[string]any{
		"team":  teamParam,
		"app":   1,
		"name":  "bar",
		"desc":  "bar desc",
		"roles": map[string]any{"read": nil, "write": nil},
	})
	require.True(t, mcResp.OK, "make_channel failed: %+v", mcResp.Error)
	var mcRes struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(mcResp.Result, &mcRes))
	require.NotEmpty(t, mcRes.ID)

	dupResp := call("make_channel", map[string]any{
		"team":  teamParam,
		"app":   1,
		"name":  "bar",
		"desc":  "dup",
		"roles": map[string]any{"read": nil, "write": nil},
	})
	require.False(t, dupResp.OK)
	require.NotNil(t, dupResp.Error)
	require.Equal(t, "channel_exists", dupResp.Error.Code)

	// 6. sync_inbox & inbox_view
	syncResp := call("sync_inbox", map[string]any{"app": 1})
	require.True(t, syncResp.OK, "sync_inbox failed: %+v", syncResp.Error)

	ivResp := call("inbox_view", map[string]any{"app": 1, "local_only": true})
	require.True(t, ivResp.OK, "inbox_view failed: %+v", ivResp.Error)
	var ivRes struct {
		Rows []struct {
			Channel struct {
				Name string `json:"name"`
			} `json:"channel"`
			Unread uint64 `json:"unread"`
		} `json:"rows"`
		Stale bool `json:"stale"`
	}
	require.NoError(t, json.Unmarshal(ivResp.Result, &ivRes))
	require.NotEmpty(t, ivRes.Rows)
	require.False(t, ivRes.Stale)

	// 7. drain & list_outbox
	drainResp := call("drain", nil)
	require.True(t, drainResp.OK, "drain failed: %+v", drainResp.Error)
	loResp := call("list_outbox", nil)
	require.True(t, loResp.OK, "list_outbox failed: %+v", loResp.Error)

	// 8. discard_outbox on bogus msg_id -> bad_args (malformed) or row_not_found (valid zero-id format)
	zeroMsgID, err := (proto.RTMsgID{}).StringErr()
	require.NoError(t, err)
	discResp := call("discard_outbox", map[string]any{"msg_id": zeroMsgID})
	require.False(t, discResp.OK)
	require.NotNil(t, discResp.Error)
	require.Equal(t, "row_not_found", discResp.Error.Code)

	// 9. run_signup was removed in wire v3 (signup is a session now).
	suResp := call("run_signup", nil)
	require.False(t, suResp.OK)
	require.NotNil(t, suResp.Error)
	require.Equal(t, "invalid_input", suResp.Error.Code)

	// 10. status reports the rig's active, unlocked user.
	stResp := call("status", nil)
	require.True(t, stResp.OK, "status failed: %+v", stResp.Error)
	var stRes struct {
		WireVersion int `json:"wire_version"`
		User        *struct {
			Username string `json:"username"`
			Unlocked bool   `json:"unlocked"`
		} `json:"user"`
	}
	require.NoError(t, json.Unmarshal(stResp.Result, &stRes))
	require.Equal(t, 3, stRes.WireVersion)
	require.NotNil(t, stRes.User)
	require.NotEmpty(t, stRes.User.Username)
	require.True(t, stRes.User.Unlocked)
}

// TestShimAdapterTeams drives the v3 team operations through the adapter
// against the rig: create, list, roster, invite, and the taken-name error.
func TestShimAdapterTeams(t *testing.T) {
	r := makeRTOutboxTestRig(t)
	ctx := r.mb.Ctx()
	sh, err := shim.NewAdapter(r.mb.Base().G())
	require.NoError(t, err)

	st, err := sh.Status(ctx)
	require.NoError(t, err)
	require.NotNil(t, st)
	require.True(t, st.Unlocked)

	rnd, err := core.RandomBase36String(6)
	require.NoError(t, err)
	name := "shimteam" + strings.ToLower(rnd)
	fqs, err := sh.CreateTeam(ctx, proto.NameUtf8(name))
	require.NoError(t, err)
	require.Equal(t, proto.FQTeamString(name), fqs)
	r.tew.DirectDoubleMerklePokeInTest(t)

	// A taken name is core.NameInUseError, classified bad_args.
	_, err = sh.CreateTeam(ctx, proto.NameUtf8(name))
	require.Error(t, err)
	code, ok := shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, shim.ErrBadArgs, code, "err=%v (%T)", err, err)

	teams, err := sh.ListTeams(ctx)
	require.NoError(t, err)
	var found *shim.TeamMembership
	for i := range teams {
		if teams[i].Team == fqs {
			found = &teams[i]
		}
	}
	require.NotNil(t, found, "new team missing from %+v", teams)
	typ, err := found.Role.GetT()
	require.NoError(t, err)
	require.Equal(t, proto.RoleType_OWNER, typ)

	parsed, err := core.ParseFQTeam(fqs)
	require.NoError(t, err)
	members, err := sh.TeamMembers(ctx, *parsed)
	require.NoError(t, err)
	require.Len(t, members, 1)
	require.Equal(t, st.Username, members[0].Name)
	require.False(t, members[0].IsTeam)

	code1, err := sh.CreateTeamInvite(ctx, *parsed)
	require.NoError(t, err)
	_, err = team.ImportTeamInvite(code1)
	require.NoError(t, err)
}

