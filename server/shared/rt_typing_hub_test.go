// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

func typingTestUID(n byte) proto.UID {
	var ret proto.UID
	ret[0] = byte(proto.EntityType_User)
	ret[1] = n
	return ret
}

const typingTestHost = core.ShortHostID(1)

func typingTestInbox(u proto.UID) RTInboxHubKey {
	return RTInboxHubKey{HostID: typingTestHost, Uid: u, App: proto.RTAppID_Chat}
}

func typingTestChan(c int64) RTTypingChannelKey {
	return RTTypingChannelKey{HostID: typingTestHost, Chid: proto.RTChannelIDShort(c)}
}

func woken(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestRTTypingHubFanOutAndExpiry(t *testing.T) {
	h := NewRTTypingHub()
	alice, bob, carol := typingTestUID(1), typingTestUID(2), typingTestUID(3)
	t0 := time.Unix(1_000_000, 0)

	bobWait := h.Subscribe(typingTestInbox(bob))
	aliceWait := h.Subscribe(typingTestInbox(alice))
	carolWait := h.Subscribe(typingTestInbox(carol))

	require.True(t, h.Admit(typingTestChan(7), alice, true, t0))
	// The sender is skipped even when the caller lists her.
	h.Publish(typingTestHost, proto.RTAppID_Chat, 7, alice, true, []proto.UID{alice, bob}, t0)

	require.True(t, woken(bobWait), "a recipient's parked poll is woken")
	require.False(t, woken(aliceWait), "the sender is not told about herself")
	require.False(t, woken(carolWait), "someone outside the audience is not woken")
	require.Equal(t, 0, h.NumWaiters(typingTestInbox(bob)), "a wake retires the waiter")

	got, cur := h.Since(typingTestInbox(bob), 0, t0.Add(time.Second))
	require.Equal(t, []proto.RTTyping{{
		Chid: 7,
		Uid:  alice,
		Ttl:  proto.ExportDurationMilli(RTTypingTTL - time.Second),
	}}, got)
	require.NotZero(t, cur)

	got, _ = h.Since(typingTestInbox(alice), 0, t0)
	require.Empty(t, got)
	got, _ = h.Since(typingTestInbox(carol), 0, t0)
	require.Empty(t, got)

	// Nothing new past the cursor: each state is handed out once per cursor.
	got, cur2 := h.Since(typingTestInbox(bob), cur, t0.Add(time.Second))
	require.Empty(t, got)
	require.Equal(t, cur, cur2)

	// A second device of bob's with its own cursor still sees it.
	got, _ = h.Since(typingTestInbox(bob), 0, t0.Add(2*time.Second))
	require.Len(t, got, 1)

	// Past the TTL the state is gone, also for a first poll.
	got, _ = h.Since(typingTestInbox(bob), 0, t0.Add(RTTypingTTL))
	require.Empty(t, got)
	_, items := h.Size()
	require.Zero(t, items, "reading an expired inbox drops it")
}

func TestRTTypingHubRateLimit(t *testing.T) {
	h := NewRTTypingHub()
	alice, bob := typingTestUID(1), typingTestUID(2)
	t0 := time.Unix(1_000_000, 0)
	ch := typingTestChan(7)

	require.True(t, h.Admit(ch, alice, true, t0))
	require.False(t, h.Admit(ch, alice, true, t0.Add(time.Second)),
		"a repeat inside the interval is dropped")
	require.False(t, h.Admit(ch, alice, true, t0.Add(RTTypingMinInterval-time.Millisecond)))
	require.True(t, h.Admit(ch, alice, true, t0.Add(RTTypingMinInterval)),
		"and admitted once the interval is over")

	// The limit is per user and per channel.
	require.True(t, h.Admit(ch, bob, true, t0.Add(time.Second)))
	require.True(t, h.Admit(typingTestChan(8), alice, true, t0.Add(time.Second)))

	// "Stopped" goes through once, and only while "typing" is live.
	t1 := t0.Add(RTTypingMinInterval + time.Second)
	require.True(t, h.Admit(ch, alice, false, t1))
	require.False(t, h.Admit(ch, alice, false, t1), "a second stop is dropped")
	require.False(t, h.Admit(ch, typingTestUID(9), false, t1),
		"a stop from someone who was not typing is dropped")
	require.False(t, h.Admit(ch, alice, true, t1.Add(time.Second)),
		"stopping does not reset the interval")
	require.True(t, h.Admit(ch, alice, true, t0.Add(2*RTTypingMinInterval)))

	// A stop after the state ran out has nothing to stop.
	late := t0.Add(2*RTTypingMinInterval + RTTypingTTL)
	require.False(t, h.Admit(ch, alice, false, late))
}

func TestRTTypingHubStop(t *testing.T) {
	h := NewRTTypingHub()
	alice, bob := typingTestUID(1), typingTestUID(2)
	t0 := time.Unix(1_000_000, 0)

	h.Publish(typingTestHost, proto.RTAppID_Chat, 7, alice, true, []proto.UID{bob}, t0)
	_, cur := h.Since(typingTestInbox(bob), 0, t0)

	wait := h.Subscribe(typingTestInbox(bob))
	h.Publish(typingTestHost, proto.RTAppID_Chat, 7, alice, false, []proto.UID{bob}, t0.Add(time.Second))
	require.True(t, woken(wait))

	got, cur2 := h.Since(typingTestInbox(bob), cur, t0.Add(time.Second))
	require.Equal(t, []proto.RTTyping{{Chid: 7, Uid: alice, Ttl: 0}}, got)
	require.Greater(t, cur2, cur)

	// A first poll learns who is typing now, not who stopped.
	got, _ = h.Since(typingTestInbox(bob), 0, t0.Add(time.Second))
	require.Empty(t, got)
}

func TestRTTypingHubCursorFromTheFuture(t *testing.T) {
	h := NewRTTypingHub()
	alice, bob := typingTestUID(1), typingTestUID(2)
	t0 := time.Unix(1_000_000, 0)

	h.Publish(typingTestHost, proto.RTAppID_Chat, 7, alice, true, []proto.UID{bob}, t0)
	// A cursor this process never issued (another process, a restart with the
	// clock set back) must not hide states behind it.
	future := proto.RTTypingCursor(uint64(t0.Add(time.Hour).UnixNano()))
	got, cur := h.Since(typingTestInbox(bob), future, t0)
	require.Len(t, got, 1)
	require.Less(t, cur, future)

	// Stamps stay strictly increasing when the clock does not move.
	h.Publish(typingTestHost, proto.RTAppID_Chat, 8, alice, true, []proto.UID{bob}, t0)
	got, cur2 := h.Since(typingTestInbox(bob), cur, t0)
	require.Len(t, got, 1)
	require.Equal(t, proto.RTChannelIDShort(8), got[0].Chid)
	require.Greater(t, cur2, cur)
}

func TestRTTypingHubBounds(t *testing.T) {
	h := NewRTTypingHub()
	t0 := time.Unix(1_000_000, 0)
	ch := typingTestChan(7)
	bob := typingTestUID(200)

	// Only so many typers per channel.
	for i := 0; i < RTTypingMaxPerChannel; i++ {
		require.True(t, h.Admit(ch, typingTestUID(byte(i)), true, t0))
	}
	require.False(t, h.Admit(ch, typingTestUID(100), true, t0),
		"a typer past the channel cap is dropped")
	require.True(t, h.Admit(typingTestChan(8), typingTestUID(100), true, t0),
		"another channel has its own cap")
	senders, _ := h.Size()
	require.Equal(t, RTTypingMaxPerChannel+1, senders, "a dropped typer leaves no entry")

	// Only so many states per recipient.
	for i := 0; i < RTTypingMaxPerInbox+10; i++ {
		h.Publish(typingTestHost, proto.RTAppID_Chat, proto.RTChannelIDShort(1000+i),
			typingTestUID(1), true, []proto.UID{bob}, t0)
	}
	_, items := h.Size()
	require.Equal(t, RTTypingMaxPerInbox, items)
	// Refreshing a state that is already held still works at the cap.
	h.Publish(typingTestHost, proto.RTAppID_Chat, 1000, typingTestUID(1), true, []proto.UID{bob},
		t0.Add(time.Second))
	_, items = h.Size()
	require.Equal(t, RTTypingMaxPerInbox, items)

	// Once those ran out there is room again, and the cap frees up too.
	t1 := t0.Add(RTTypingTTL + time.Second)
	require.True(t, h.Admit(ch, typingTestUID(100), true, t1))
	h.Publish(typingTestHost, proto.RTAppID_Chat, 5000, typingTestUID(1), true, []proto.UID{bob}, t1)
	got, _ := h.Since(typingTestInbox(bob), 0, t1)
	require.Len(t, got, 1)

	// And with nobody typing any more, the next call sweeps everything out:
	// nothing is left behind by members who never poll.
	t2 := t1.Add(RTTypingTTL + time.Second)
	require.False(t, h.Admit(ch, typingTestUID(101), false, t2))
	senders, items = h.Size()
	require.Zero(t, senders)
	require.Zero(t, items)
}

func TestRTTypingHubUnsubscribe(t *testing.T) {
	h := NewRTTypingHub()
	bob := typingTestUID(2)
	k := typingTestInbox(bob)
	a, b := h.Subscribe(k), h.Subscribe(k)
	require.Equal(t, 2, h.NumWaiters(k))
	h.Unsubscribe(k, a)
	require.Equal(t, 1, h.NumWaiters(k))
	h.Publish(typingTestHost, proto.RTAppID_Chat, 7, typingTestUID(1), true, []proto.UID{bob},
		time.Unix(1_000_000, 0))
	require.True(t, woken(b))
	require.False(t, woken(a))
	h.Unsubscribe(k, b) // after a wake: a no-op
	require.Equal(t, 0, h.NumWaiters(k))
}
