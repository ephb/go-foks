// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"
	"time"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// Typing into a channel whose audience is over the limit reaches nobody, and
// the server remembers that instead of reading the roster on every call.
func TestRTTypingTooLargeIsRemembered(t *testing.T) {
	sc := setupTypingScene(t)
	g := sc.tew.MetaContext().G()
	hub := g.RTTypingHub()
	boHead := sc.head(t, sc.bo)
	sc.head(t, sc.ada)

	// ada's team has three members: with room for one reader, it is too large.
	old := hub.SetMaxAudienceForTest(1)
	defer hub.SetMaxAudienceForTest(old)

	key := shared.RTTypingChannelKey{HostID: sc.tew.MetaContext().ShortHostID(), Chid: sc.chid.Short()}
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, sc.chid, true))
	res, err := sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, 0,
		300*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, res.Typing)

	// With the limit back, the mark alone keeps the channel quiet: the call
	// is accepted and stops at the hub, before any roster read.
	hub.SetMaxAudienceForTest(old)
	now := g.Clock().Now()
	require.False(t, hub.Admit(key, sc.cy.user.uid, true, now), "the channel is marked too large")
	require.NoError(t, sc.cy.minder.SetTypingByID(sc.cy.m, sc.chid, true))
	res, err = sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, 0,
		300*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, res.Typing)

	// Another channel of a team that fits is not affected.
	other := sc.makeChannel(t, "fits-", proto.DefaultRole)
	boHead = sc.head(t, sc.bo)
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, other, true))
	res, err = sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, 0, 5*time.Second)
	require.NoError(t, err)
	require.Len(t, res.Typing, 1)
	require.Equal(t, other.Short(), res.Typing[0].Chid)
}
