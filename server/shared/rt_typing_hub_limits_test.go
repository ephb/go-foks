// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// One member of many channels cannot fan out more often than the per-sender
// budget allows, whichever channels and whichever of "typing" and "stopped"
// it uses.
func TestRTTypingHubSenderBudget(t *testing.T) {
	h := NewRTTypingHub()
	alice, bob := typingTestUID(1), typingTestUID(2)
	t0 := time.Unix(1_000_000, 0)

	// On, off, on, off ... over twenty channels, all at once.
	admitted := 0
	for c := int64(1); c <= 20; c++ {
		if h.Admit(typingTestChan(c), alice, true, t0) {
			admitted++
		}
		if h.Admit(typingTestChan(c), alice, false, t0) {
			admitted++
		}
	}
	require.Equal(t, RTTypingMaxPerSender, admitted)

	// Still nothing just before the interval is over, in old and new channels.
	almost := t0.Add(RTTypingMinInterval - time.Millisecond)
	require.False(t, h.Admit(typingTestChan(50), alice, true, almost))
	require.False(t, h.Admit(typingTestChan(3), alice, true, almost))

	// Someone else is not held back by her.
	require.True(t, h.Admit(typingTestChan(50), bob, true, almost))

	// A whole interval later the budget is there again, and no more than it.
	t1 := t0.Add(RTTypingMinInterval)
	admitted = 0
	for c := int64(100); c < 120; c++ {
		if h.Admit(typingTestChan(c), alice, true, t1) {
			admitted++
		}
	}
	require.Equal(t, RTTypingMaxPerSender, admitted)

	// Over a longer time: at most the budget per interval, however she
	// spreads the calls.
	h = NewRTTypingHub()
	admitted = 0
	const intervals = 10
	for ms := 0; ms < intervals*int(RTTypingMinInterval/time.Millisecond); ms += 50 {
		now := t0.Add(time.Duration(ms) * time.Millisecond)
		c := typingTestChan(int64(ms/50) % 40)
		if h.Admit(c, alice, true, now) {
			admitted++
		}
		if h.Admit(c, alice, false, now) {
			admitted++
		}
	}
	require.LessOrEqual(t, admitted, intervals*RTTypingMaxPerSender)
	require.Greater(t, admitted, 0)

	// The budget entries go with the sweep once they no longer count.
	late := t0.Add(time.Hour)
	require.False(t, h.Admit(typingTestChan(7), bob, false, late))
	budgets, _ := h.sizeLimits()
	require.Zero(t, budgets)
}

// A "stopped" that the budget refuses leaves the state as it was, so the
// channel cap and a later stop still see it.
func TestRTTypingHubStopOutOfBudget(t *testing.T) {
	h := NewRTTypingHub()
	alice := typingTestUID(1)
	t0 := time.Unix(1_000_000, 0)
	for c := int64(1); c <= RTTypingMaxPerSender; c++ {
		require.True(t, h.Admit(typingTestChan(c), alice, true, t0))
	}
	require.False(t, h.Admit(typingTestChan(1), alice, false, t0.Add(time.Second)),
		"no budget left for the stop")
	require.True(t, h.Admit(typingTestChan(1), alice, false, t0.Add(RTTypingMinInterval)),
		"the stop goes through once there is budget again")
}

// The channel cap counts who is typing now: members who stopped make room.
func TestRTTypingHubChannelCapCountsOnlyTypers(t *testing.T) {
	h := NewRTTypingHub()
	t0 := time.Unix(1_000_000, 0)
	ch := typingTestChan(7)
	for i := 0; i < RTTypingMaxPerChannel; i++ {
		require.True(t, h.Admit(ch, typingTestUID(byte(i)), true, t0))
	}
	require.False(t, h.Admit(ch, typingTestUID(100), true, t0))

	t1 := t0.Add(time.Second)
	require.True(t, h.Admit(ch, typingTestUID(0), false, t1))
	require.True(t, h.Admit(ch, typingTestUID(100), true, t1),
		"a member who stopped no longer takes a place")
	require.False(t, h.Admit(ch, typingTestUID(101), true, t1))
}

// A poll answers with typing states alone at most once per
// RTTypingMinReturnInterval for an inbox.
func TestRTTypingHubReturnDelay(t *testing.T) {
	h := NewRTTypingHub()
	bob, carol := typingTestInbox(typingTestUID(2)), typingTestInbox(typingTestUID(3))
	t0 := time.Unix(1_000_000, 0)

	require.Zero(t, h.ReturnDelay(bob, t0), "the first result is not held")
	h.NoteReturn(bob, t0)
	require.Equal(t, RTTypingMinReturnInterval, h.ReturnDelay(bob, t0))
	require.Equal(t, 400*time.Millisecond, h.ReturnDelay(bob, t0.Add(600*time.Millisecond)))
	require.Zero(t, h.ReturnDelay(bob, t0.Add(RTTypingMinReturnInterval)))
	require.Zero(t, h.ReturnDelay(carol, t0), "another inbox is not held")
	require.Zero(t, h.ReturnDelay(bob, t0.Add(-time.Minute)), "a clock set back does not hold a poll")

	// The entry goes with the sweep.
	h.NoteReturn(carol, t0.Add(time.Hour))
	_, returned := h.sizeLimits()
	require.Equal(t, 1, returned)
}
