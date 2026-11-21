// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// rtSetChannelFlags: a user's own muted and hidden flags for a channel.
package lib

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// flagsOf syncs a's inbox and returns the channel's row.
func (s *typingScene) flagsOf(t *testing.T, a *typingActor, chid proto.RTChannelID) lcl.RTInboxRowView {
	_, err := a.minder.SyncInbox(a.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	view, err := a.minder.LocalInbox(a.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	for _, row := range view.Rows {
		if row.Ch.Id.Eq(chid) {
			return row
		}
	}
	require.Fail(t, "the channel is not in the inbox")
	return lcl.RTInboxRowView{}
}

func TestRTSetChannelFlags(t *testing.T) {
	sc := setupTypingScene(t)
	yes, no := true, false
	set := func(a *typingActor, muted, hidden *bool) error {
		return a.minder.SetChannelFlagsByID(a.m, sc.chid, muted, hidden)
	}

	row := sc.flagsOf(t, sc.bo, sc.chid)
	require.False(t, row.Muted)
	require.False(t, row.Hidden)

	// A second client of bo's (its own local state, as another device has).
	bo2m := librt.NewMetaContext(sc.tew.NewClientMetaContextWithEracer(t, sc.bo.user))
	bo2 := &typingActor{user: sc.bo.user, m: bo2m, minder: librt.NewMinder(bo2m.G().ActiveUser())}
	head := sc.head(t, bo2)

	// bo mutes. His parked poll (the other device's) is woken, and both
	// clients see the flag after a sync. Hidden is left alone.
	poll := make(chan typingPoll, 1)
	go func() {
		r, e := bo2.minder.PollInbox(bo2.m, proto.RTAppID_Chat, head, 10*time.Second)
		poll <- typingPoll{res: r, err: e}
	}()
	g := sc.tew.MetaContext().G()
	require.Eventually(t, func() bool {
		return g.RTInboxHub().NumWaiters(sc.inboxKey(sc.bo)) > 0
	}, 10*time.Second, 5*time.Millisecond)
	start := time.Now()
	require.NoError(t, set(sc.bo, &yes, nil))
	res := sc.takePoll(t, poll)
	require.Less(t, time.Since(start), 5*time.Second)
	require.True(t, res.Bumped, "a changed flag bumps the caller's inbox")
	require.Greater(t, res.InboxVersion, head)

	row = sc.flagsOf(t, sc.bo, sc.chid)
	require.True(t, row.Muted)
	require.False(t, row.Hidden)
	row = sc.flagsOf(t, bo2, sc.chid)
	require.True(t, row.Muted, "the flag reaches the user's other client")
	require.False(t, row.Hidden)

	// The flags are bo's own: cy and ada see nothing.
	for _, other := range []*typingActor{sc.cy, sc.ada} {
		row = sc.flagsOf(t, other, sc.chid)
		require.False(t, row.Muted)
		require.False(t, row.Hidden)
	}

	// The same value again changes nothing and bumps nothing.
	before, err := sc.bo.minder.GetInboxVersion(sc.bo.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.NoError(t, set(sc.bo, &yes, nil))
	require.NoError(t, set(sc.bo, &yes, &no))
	after, err := sc.bo.minder.GetInboxVersion(sc.bo.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	require.Equal(t, before, after)

	// Both at once, then each back on its own.
	require.NoError(t, set(sc.bo, &no, &yes))
	row = sc.flagsOf(t, bo2, sc.chid)
	require.False(t, row.Muted)
	require.True(t, row.Hidden)
	require.NoError(t, set(sc.bo, nil, &no))
	row = sc.flagsOf(t, bo2, sc.chid)
	require.False(t, row.Muted)
	require.False(t, row.Hidden)

	// By team and channel name too.
	require.NoError(t, sc.cy.minder.SetChannelFlags(sc.cy.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(sc.chid), nil, &yes))
	require.True(t, sc.flagsOf(t, sc.cy, sc.chid).Hidden)
	require.False(t, sc.flagsOf(t, sc.bo, sc.chid).Hidden)

	// The flags survive messages and read marks.
	require.NoError(t, set(sc.bo, &yes, nil))
	_, err = sc.ada.minder.Send(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(sc.chid), []byte("hello"))
	require.NoError(t, err)
	row = sc.flagsOf(t, sc.bo, sc.chid)
	require.True(t, row.Muted)
	require.Equal(t, proto.RTMsgSeq(1), row.LastSeq)
}

func TestRTSetChannelFlagsRefusals(t *testing.T) {
	sc := setupTypingScene(t)
	yes := true

	// Neither flag: nothing to do.
	err := sc.bo.minder.SetChannelFlagsByID(sc.bo.m, sc.chid, nil, nil)
	require.Error(t, err)
	require.IsType(t, core.BadArgsError(""), err)

	// dee is in no team.
	err = sc.dee.minder.SetChannelFlagsByID(sc.dee.m, sc.chid, &yes, nil)
	require.Error(t, err)
	require.True(t, core.IsPermissionError(err), "got %v", err)

	// A channel only admins can read: bo may not touch it, ada may.
	adm, err := sc.ada.minder.MakeChannel(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat,
		rtRandomChannelName(t, "admins-"), "admins only",
		proto.RolePairOpt{Read: &proto.AdminRole, Write: &proto.AdminRole})
	require.NoError(t, err)
	err = sc.bo.minder.SetChannelFlagsByID(sc.bo.m, *adm, &yes, nil)
	require.Error(t, err)
	require.True(t, core.IsPermissionError(err), "got %v", err)
	require.NoError(t, sc.ada.minder.SetChannelFlagsByID(sc.ada.m, *adm, &yes, nil))
	require.True(t, sc.flagsOf(t, sc.ada, *adm).Muted)

	// A channel that does not exist.
	var nope proto.RTChannelID
	copy(nope[:], sc.chid[:])
	nope[1] ^= 0xff
	nope[2] ^= 0xff
	err = sc.bo.minder.SetChannelFlagsByID(sc.bo.m, nope, &yes, nil)
	require.Error(t, err)

	// Nobody else's flags moved.
	row := sc.flagsOf(t, sc.bo, sc.chid)
	require.False(t, row.Muted)
	require.False(t, row.Hidden)
}

// A hidden channel comes back with its next message, for whoever hid it;
// a read mark does not bring it back, and muted stays.
func TestRTHiddenUntilNextMessage(t *testing.T) {
	sc := setupTypingScene(t)
	yes := true
	spec := lcl.NewRTChannelSpecifierWithId(sc.chid)
	send := func(a *typingActor, text string) {
		_, err := a.minder.Send(a.m, sc.teamCfg(), proto.RTAppID_Chat, spec, []byte(text))
		require.NoError(t, err)
	}
	send(sc.ada, "one")

	require.NoError(t, sc.bo.minder.SetChannelFlagsByID(sc.bo.m, sc.chid, &yes, &yes))
	require.NoError(t, sc.ada.minder.SetChannelFlagsByID(sc.ada.m, sc.chid, nil, &yes))
	row := sc.flagsOf(t, sc.bo, sc.chid)
	require.True(t, row.Hidden)
	require.True(t, row.Muted)

	// Reading what is there is not news.
	_, err := sc.bo.minder.GetThreadView(sc.bo.m, sc.teamCfg(), proto.RTAppID_Chat, spec, 0, 10)
	require.NoError(t, err)
	row = sc.flagsOf(t, sc.bo, sc.chid)
	require.True(t, row.Hidden, "a read mark must not bring the channel back")

	send(sc.cy, "two")
	row = sc.flagsOf(t, sc.bo, sc.chid)
	require.False(t, row.Hidden, "the next message brings the channel back")
	require.True(t, row.Muted, "muted is not touched by a message")
	require.False(t, sc.flagsOf(t, sc.ada, sc.chid).Hidden)

	// Hiding again works, and the hider's own message brings it back too.
	require.NoError(t, sc.bo.minder.SetChannelFlagsByID(sc.bo.m, sc.chid, nil, &yes))
	require.True(t, sc.flagsOf(t, sc.bo, sc.chid).Hidden)
	send(sc.bo, "three")
	require.False(t, sc.flagsOf(t, sc.bo, sc.chid).Hidden)
}

// A flag change and a send into the same channel take their row locks in the
// same order (user_inbox, then user_channels), so neither is aborted as a
// deadlock victim: every call of both succeeds while they race.
func TestRTSetChannelFlagsRacesSend(t *testing.T) {
	sc := setupTypingScene(t)
	yes := true
	spec := lcl.NewRTChannelSpecifierWithId(sc.chid)
	_, err := sc.ada.minder.Send(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat, spec, []byte("first"))
	require.NoError(t, err)

	const n = 40
	sendErr := make(chan error, 1)
	go func() {
		for range n {
			_, err := sc.ada.minder.Send(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat, spec, []byte("m"))
			if err != nil {
				sendErr <- err
				return
			}
		}
		sendErr <- nil
	}()
	// Each send clears hidden again, so most of these are real changes.
	for range n {
		require.NoError(t, sc.bo.minder.SetChannelFlagsByID(sc.bo.m, sc.chid, nil, &yes))
	}
	require.NoError(t, <-sendErr)
}
