// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/stretchr/testify/require"
)

// A member who switches typing on and off in many channels cannot keep
// another member's poll loop turning: the server fans out only so many of
// that member's states per interval, and a poll answers with typing states
// alone at most once per shared.RTTypingMinReturnInterval.
func TestRTTypingCannotKeepPollsBusy(t *testing.T) {
	sc := setupTypingScene(t)
	chans := []proto.RTChannelID{sc.chid}
	for i := 0; i < 5; i++ {
		chans = append(chans, sc.makeChannel(t, "busy-", proto.DefaultRole))
	}
	boHead := sc.head(t, sc.bo)
	sc.head(t, sc.ada)

	// bo polls again as soon as each poll returns, as a client does.
	stop := make(chan struct{})
	done := make(chan struct{})
	var returns []time.Time
	states := 0
	go func() {
		defer close(done)
		var cursor proto.RTTypingCursor
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := sc.bo.minder.PollInboxTyping(sc.bo.m, proto.RTAppID_Chat, boHead, cursor,
				500*time.Millisecond)
			if err != nil {
				return
			}
			if res.Bumped {
				boHead = res.InboxVersion
			}
			cursor = res.TypingCursor
			if len(res.Typing) > 0 {
				returns = append(returns, time.Now())
				states += len(res.Typing)
			}
		}
	}()

	// ada: on and off in every channel, round after round, for 3.5 seconds.
	start := time.Now()
	const run = 3500 * time.Millisecond
	for time.Since(start) < run {
		for _, c := range chans {
			require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, c, true))
			require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, c, false))
		}
	}
	elapsed := time.Since(start)
	close(stop)
	<-done

	require.NotEmpty(t, returns, "typing still arrives")
	// One return per started interval, and one for the edge.
	maxReturns := int(elapsed/shared.RTTypingMinReturnInterval) + 2
	require.LessOrEqual(t, len(returns), maxReturns,
		"typing-only poll results are spaced out")
	for i := 1; i < len(returns); i++ {
		require.GreaterOrEqual(t, returns[i].Sub(returns[i-1]),
			shared.RTTypingMinReturnInterval-150*time.Millisecond)
	}
	// And the states themselves are capped per sender, not per channel.
	maxStates := (int(elapsed/shared.RTTypingMinInterval) + 2) * shared.RTTypingMaxPerSender
	require.LessOrEqual(t, states, maxStates)
}

// A message still wakes a poll that is holding typing states back.
func TestRTTypingHeldPollStillWakesForAMessage(t *testing.T) {
	sc := setupTypingScene(t)
	other := sc.makeChannel(t, "other-", proto.DefaultRole)
	boHead := sc.head(t, sc.bo)
	sc.head(t, sc.ada)

	boPoll := sc.parkTypingPoll(t, sc.bo, boHead, 0)
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, sc.chid, true))
	res := sc.takePoll(t, boPoll)
	require.Len(t, res.Typing, 1)

	// Inside the interval: a second state is held back, a message is not.
	boPoll = sc.parkTypingPoll(t, sc.bo, boHead, res.TypingCursor)
	require.NoError(t, sc.ada.minder.SetTypingByID(sc.ada.m, other, true))
	start := time.Now()
	_, err := sc.ada.minder.Send(sc.ada.m, sc.teamCfg(), proto.RTAppID_Chat,
		lcl.NewRTChannelSpecifierWithId(sc.chid), []byte("hello"))
	require.NoError(t, err)
	res = sc.takePoll(t, boPoll)
	require.True(t, res.Bumped)
	require.Less(t, time.Since(start), shared.RTTypingMinReturnInterval-200*time.Millisecond,
		"the bump does not wait for the typing interval")
	require.Len(t, res.Typing, 1, "and brings the held state along")
	require.Equal(t, other.Short(), res.Typing[0].Chid)
}
