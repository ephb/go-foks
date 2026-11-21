// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

// Typing states: rtSetTyping and the typing side of rtPollInbox.
//
// A typing state is a hint that the server never stores. These tests cover
// what the server must get right all the same: who may send one (exactly who
// may send a message), who is told (the channel's other readers, and only
// polls that asked), and that none of it disturbs message delivery. The
// rate limit, the bounds and the expiry are unit-tested with a fake clock in
// server/shared/rt_typing_hub_test.go.
package lib

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/librt"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

type typingActor struct {
	user   *TestUser
	m      librt.MetaContext
	minder *librt.Minder
}

// typingScene is a team with an admin (ada) and two ordinary members (bo and
// cy), a channel all three can read and write, and dee, who is in no team.
type typingScene struct {
	tew              *TestEnvWrapper
	tm               *teamObj
	fqt              *proto.FQTeamParsed
	ada, bo, cy, dee *typingActor
	chid             proto.RTChannelID
}

func setupTypingScene(t *testing.T) *typingScene {
	tew := testEnvBeta(t)
	users := make([]*TestUser, 4)
	for i := range users {
		users[i] = tew.NewTestUser(t)
	}
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, users[0])
	tm.makeChanges(t, tew.MetaContext(), users[0],
		[]proto.MemberRole{
			users[1].toMemberRole(t, proto.DefaultRole, tm.hepks),
			users[2].toMemberRole(t, proto.DefaultRole, tm.hepks),
		}, nil)

	actor := func(u *TestUser) *typingActor {
		m := librt.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
		return &typingActor{user: u, m: m, minder: librt.NewMinder(m.G().ActiveUser())}
	}
	sc := &typingScene{
		tew: tew,
		tm:  tm,
		fqt: tm.ToFQTeamParsed(t),
		ada: actor(users[0]),
		bo:  actor(users[1]),
		cy:  actor(users[2]),
		dee: actor(users[3]),
	}
	sc.chid = sc.makeChannel(t, "talk-", proto.DefaultRole)
	return sc
}

func (s *typingScene) teamCfg() lcl.ConfigTeam { return team.WrapNamedPtr(s.fqt) }

func (s *typingScene) makeChannel(t *testing.T, prefix string, write proto.Role) proto.RTChannelID {
	chid, err := s.ada.minder.MakeChannel(s.ada.m, s.teamCfg(), proto.RTAppID_Chat,
		rtRandomChannelName(t, prefix), "a channel",
		proto.RolePairOpt{Read: &proto.DefaultRole, Write: &write})
	require.NoError(t, err)
	return *chid
}

func (s *typingScene) inboxKey(a *typingActor) shared.RTInboxHubKey {
	return shared.RTInboxHubKey{
		HostID: s.tew.MetaContext().ShortHostID(),
		Uid:    a.user.uid,
		App:    proto.RTAppID_Chat,
	}
}

// head is a's current inbox version, after the sync that fans a into the
// team's channels.
func (s *typingScene) head(t *testing.T, a *typingActor) proto.RTInboxVersion {
	sum, err := a.minder.SyncInbox(a.m, proto.RTAppID_Chat)
	require.NoError(t, err)
	return sum.Vers
}

type typingPoll struct {
	res *proto.RTInboxPollRes
	err error
}

// parkTypingPoll starts a typing poll for a and returns once the server has
// it parked on both hubs, so that what the test does next finds a waiter.
func (s *typingScene) parkTypingPoll(
	t *testing.T,
	a *typingActor,
	since proto.RTInboxVersion,
	cursor proto.RTTypingCursor,
) chan typingPoll {
	ch := make(chan typingPoll, 1)
	go func() {
		r, e := a.minder.PollInboxTyping(a.m, proto.RTAppID_Chat, since, cursor, 10*time.Second)
		ch <- typingPoll{res: r, err: e}
	}()
	g := s.tew.MetaContext().G()
	key := s.inboxKey(a)
	require.Eventually(t, func() bool {
		return g.RTTypingHub().NumWaiters(key) > 0 && g.RTInboxHub().NumWaiters(key) > 0
	}, 10*time.Second, 5*time.Millisecond)
	return ch
}

func (s *typingScene) takePoll(t *testing.T, ch chan typingPoll) *proto.RTInboxPollRes {
	select {
	case out := <-ch:
		require.NoError(t, out.err)
		return out.res
	case <-time.After(5 * time.Second):
		require.Fail(t, "the parked poll was not woken")
		return nil
	}
}

func TestRTTypingReachesOtherMembers(t *testing.T) {
	sc := setupTypingScene(t)
	boHead := sc.head(t, sc.bo)
	adaHead := sc.head(t, sc.ada)
	cyHead := sc.head(t, sc.cy)

	// bo asks for typing; cy polls the old way and must be left alone.
	boPoll := sc.parkTypingPoll(t, sc.bo, boHead, 0)
	cyPoll := make(chan typingPoll, 1)
	go func() {
		r, e := sc.cy.minder.PollInbox(sc.cy.m, proto.RTAppID_Chat, cyHead, 2*time.Second)
		cyPoll <- typingPoll{res: r, err: e}
	}()
	g := sc.tew.MetaContext().G()
	require.Eventually(t, func() bool {
		return g.RTInboxHub().NumWaiters(sc.inboxKey(sc.cy)) > 0
	}, 10*time.Second, 5*time.Millisecond)
	require.Zero(t, g.RTTypingHub().NumWaiters(sc.inboxKey(sc.cy)),
		"a poll that did not ask for typing must not park on the typing hub")

	start := time.Now()
	require.NoError(t, sc.ada.minder.SetTyping(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(sc.chid), true))

	res := sc.takePoll(t, boPoll)
	require.Less(t, time.Since(start), 2*time.Second, "the state arrives by wake, not by timeout")
	require.False(t, res.Bumped, "typing is not an inbox bump")
	require.Equal(t, boHead, res.InboxVersion)
	require.Len(t, res.Typing, 1)
	require.Equal(t, sc.chid.Short(), res.Typing[0].Chid)
	require.Equal(t, sc.ada.user.uid, res.Typing[0].Uid)
	ttl := res.Typing[0].Ttl.Duration()
	require.Greater(t, ttl, shared.RTTypingTTL-5*time.Second)
	require.LessOrEqual(t, ttl, shared.RTTypingTTL)
	require.NotZero(t, res.TypingCursor)
	cursor := res.TypingCursor

	// cy's plain poll ran to its timeout, untouched.
	out := <-cyPoll
	require.NoError(t, out.err)
	require.False(t, out.res.Bumped)
	require.Empty(t, out.res.Typing)
	require.Zero(t, out.res.TypingCursor)

	// ada is not told about herself.
	own, err := sc.ada.minder.PollInboxTyping(sc.ada.m, proto.RTAppID_Chat, adaHead, 0,
		200*time.Millisecond)
	require.NoError(t, err)
	require.False(t, own.Bumped)
	require.Empty(t, own.Typing)

	// Past the cursor there is nothing new for bo, and a repeat inside the
	// rate limit is accepted without a second fan-out.
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, sc.chid, true))
	again, err := sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, cursor,
		300*time.Millisecond)
	require.NoError(t, err)
	require.False(t, again.Bumped)
	require.Empty(t, again.Typing)
	require.Equal(t, cursor, again.TypingCursor)

	// A poll that starts from scratch learns who is typing right now.
	fresh, err := sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, 0,
		10*time.Second)
	require.NoError(t, err)
	require.Len(t, fresh.Typing, 1)

	// "Stopped" goes out at once, with no time left.
	boPoll = sc.parkTypingPoll(t, sc.bo, boHead, cursor)
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, sc.chid, false))
	res = sc.takePoll(t, boPoll)
	require.False(t, res.Bumped)
	require.Len(t, res.Typing, 1)
	require.Equal(t, sc.ada.user.uid, res.Typing[0].Uid)
	require.Zero(t, res.Typing[0].Ttl)
	require.Greater(t, res.TypingCursor, cursor)
	cursor = res.TypingCursor

	// Message delivery is what it was: a parked typing poll is woken by a
	// send and reports the bump.
	boPoll = sc.parkTypingPoll(t, sc.bo, boHead, cursor)
	_, err = sc.ada.minder.Send(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(sc.chid), []byte("done typing"))
	require.NoError(t, err)
	res = sc.takePoll(t, boPoll)
	require.True(t, res.Bumped)
	require.Greater(t, res.InboxVersion, boHead)
	require.Empty(t, res.Typing)

	// Nothing was written for any of it.
	db, err := sc.tew.MetaContext().Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer db.Release()
	var n int
	require.NoError(t, db.QueryRow(sc.tew.MetaContext().Ctx(),
		`SELECT count(*) FROM messages_enc WHERE short_host_id=$1 AND channel_id=$2`,
		sc.tew.MetaContext().ShortHostID(), sc.chid.Short().Int64()).Scan(&n))
	require.Equal(t, 1, n, "only the one real message is in the channel")
}

// Typing is authorized exactly as a send is.
func TestRTTypingNeedsTheRightToSend(t *testing.T) {
	sc := setupTypingScene(t)
	boHead := sc.head(t, sc.bo)

	// dee is in no team: refused, and bo hears nothing.
	err := sc.dee.minder.SetTypingByID(sc.dee.m, sc.chid, true)
	require.Error(t, err)
	require.True(t, core.IsPermissionError(err), "got %v", err)

	// A channel only admins may write to: bo can read it, and may not type.
	ann := sc.makeChannel(t, "announce-", proto.AdminRole)
	boHead = sc.head(t, sc.bo)
	err = sc.bo.minder.SetTypingByID(sc.bo.m, ann, true)
	require.Error(t, err)
	require.True(t, core.IsPermissionError(err), "got %v", err)

	res, err := sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, 0,
		300*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, res.Typing, "a refused typing call must not be fanned out")

	// ada may, and bo, a reader, is told.
	boPoll := sc.parkTypingPoll(t, sc.bo, boHead, res.TypingCursor)
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, ann, true))
	got := sc.takePoll(t, boPoll)
	require.Len(t, got.Typing, 1)
	require.Equal(t, ann.Short(), got.Typing[0].Chid)

	// A channel that does not exist.
	bogus, err := proto.NewRTChannelID()
	require.NoError(t, err)
	err = sc.ada.minder.SetTypingByID(sc.ada.m, *bogus, true)
	require.Error(t, err)
	require.IsType(t, core.RowNotFoundError{}, err)

	// An archived channel is closed to typing as it is to sending, and a
	// caller who could not send learns nothing new from the refusal.
	require.NoError(t, sc.ada.minder.SetChannelArchived(sc.ada.m, sc.teamCfg(),
		proto.RTAppID_Chat, lcl.NewRTChannelSpecifierWithId(ann), true))
	require.IsType(t, core.RTChannelArchivedError{}, sc.ada.minder.SetTypingByID(sc.ada.m, ann, true))
	err = sc.bo.minder.SetTypingByID(sc.bo.m, ann, true)
	require.True(t, core.IsPermissionError(err), "got %v", err)
}

// A member below a channel's read role is not in its audience.
func TestRTTypingStaysInsideTheReadRole(t *testing.T) {
	sc := setupTypingScene(t)
	brass, err := sc.ada.minder.MakeChannel(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat,
		rtRandomChannelName(t, "brass-"), "admins only",
		proto.RolePairOpt{Read: &proto.AdminRole, Write: &proto.AdminRole})
	require.NoError(t, err)
	boHead := sc.head(t, sc.bo)

	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, *brass, true))
	res, err := sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, 0,
		300*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, res.Typing, "typing in a channel bo cannot read must not reach bo")
}
