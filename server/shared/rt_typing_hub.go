// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"sync"
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

// Limits of the typing fan-out (rtSetTyping). A typing state is a hint: it
// is never stored, and everything here is sized so that a hostile client can
// neither grow this process's memory nor keep other members' polls busy.
const (
	// RTTypingTTL is how long a typing state holds without being repeated.
	RTTypingTTL = 10 * time.Second
	// RTTypingMinInterval is the least time between two fan-outs of "typing"
	// for one user in one channel. A call inside it is accepted and dropped.
	RTTypingMinInterval = 3 * time.Second
	// RTTypingMaxPerChannel caps how many users are typing in one channel at
	// once, as far as the fan-out goes. Past it a new typer is dropped; a
	// client shows "several people are typing" long before.
	RTTypingMaxPerChannel = 16
	// RTTypingMaxPerInbox caps the states held for one recipient.
	RTTypingMaxPerInbox = 64
	// RTTypingMaxAudience is the largest number of readers a typing state is
	// fanned out to. A channel with more gets none.
	RTTypingMaxAudience = 256

	rtTypingSweepEvery = RTTypingTTL
)

// RTTypingChannelKey identifies a channel across virtual hosts.
type RTTypingChannelKey struct {
	HostID core.ShortHostID
	Chid   proto.RTChannelIDShort
}

type rtTypingSender struct {
	lastOn time.Time // the last fan-out of "typing"
	until  time.Time // when that state runs out
	on     bool      // false once "stopped" was fanned out
}

type rtTypingItemKey struct {
	chid proto.RTChannelIDShort
	uid  proto.UID
}

type rtTypingItem struct {
	stamp uint64
	on    bool
	until time.Time // on: the state's end; off: when the "stopped" is dropped
}

// RTTypingHub hands typing states (rtSetTyping) to the parked long-polls of
// the other members (rtPollInbox with typing set). It has its own waiters,
// apart from RTInboxHub's, so that a typing state never wakes a poll that
// did not ask for typing, and never costs a woken poll a database read.
//
// A recipient reads its states by cursor rather than by draining a queue, so
// that each of a user's devices sees every state. The cursor is a stamp from
// this process's wall clock, made strictly increasing.
//
// In-process only, like RTInboxHub: a state published on one realtime server
// process is not seen by a poll parked on another. Unlike an inbox bump there
// is nothing to catch up on later, so with more than one process typing
// states are simply missed across them until a shared wake source exists.
//
// All times come from the caller, which keeps the hub testable with a fake
// clock.
type RTTypingHub struct {
	sync.Mutex
	senders   map[RTTypingChannelKey]map[proto.UID]*rtTypingSender
	inboxes   map[RTInboxHubKey]map[rtTypingItemKey]rtTypingItem
	waiters   map[RTInboxHubKey]map[chan struct{}]bool
	lastStamp uint64
	lastSweep time.Time
}

func NewRTTypingHub() *RTTypingHub {
	return &RTTypingHub{
		senders: make(map[RTTypingChannelKey]map[proto.UID]*rtTypingSender),
		inboxes: make(map[RTInboxHubKey]map[rtTypingItemKey]rtTypingItem),
		waiters: make(map[RTInboxHubKey]map[chan struct{}]bool),
	}
}

// Admit is the rate limit: it reports whether uid's new state in the channel
// is to be fanned out, and records it if so. Call it only for a caller
// already authorized for the channel, so that an entry can only be made for
// a channel the caller may write to.
//
// "Typing" is admitted at most once per RTTypingMinInterval per user and
// channel, and not for a new typer while RTTypingMaxPerChannel others are
// live. "Stopped" is admitted once, and only while the user's "typing" is
// still live.
func (h *RTTypingHub) Admit(k RTTypingChannelKey, uid proto.UID, on bool, now time.Time) bool {
	h.Lock()
	defer h.Unlock()
	h.sweepLocked(now)

	set := h.senders[k]
	s := set[uid]
	if !on {
		if s == nil || !s.on || !now.Before(s.until) {
			return false
		}
		s.on = false
		return true
	}
	if s != nil && now.Sub(s.lastOn) < RTTypingMinInterval {
		return false
	}
	if s == nil {
		live := 0
		for _, o := range set {
			if now.Before(o.until) {
				live++
			}
		}
		if live >= RTTypingMaxPerChannel {
			return false
		}
		if set == nil {
			set = make(map[proto.UID]*rtTypingSender)
			h.senders[k] = set
		}
		s = &rtTypingSender{}
		set[uid] = s
	}
	s.lastOn = now
	s.until = now.Add(RTTypingTTL)
	s.on = true
	return true
}

// Publish records from's state for each recipient and wakes their parked
// polls. from is skipped if listed. A recipient already holding
// RTTypingMaxPerInbox live states does not get this one.
func (h *RTTypingHub) Publish(
	host core.ShortHostID,
	app proto.RTAppID,
	chid proto.RTChannelIDShort,
	from proto.UID,
	on bool,
	recipients []proto.UID,
	now time.Time,
) {
	h.Lock()
	defer h.Unlock()

	stamp := uint64(now.UnixNano())
	if stamp <= h.lastStamp {
		stamp = h.lastStamp + 1
	}
	h.lastStamp = stamp

	ik := rtTypingItemKey{chid: chid, uid: from}
	item := rtTypingItem{stamp: stamp, on: on, until: now.Add(RTTypingTTL)}
	for _, uid := range recipients {
		if uid == from {
			continue
		}
		k := RTInboxHubKey{HostID: host, Uid: uid, App: app}
		box := h.inboxes[k]
		if _, replaces := box[ik]; !replaces && len(box) >= RTTypingMaxPerInbox {
			pruneTypingInbox(box, now)
			if len(box) >= RTTypingMaxPerInbox {
				continue
			}
		}
		if box == nil {
			box = make(map[rtTypingItemKey]rtTypingItem)
			h.inboxes[k] = box
		}
		box[ik] = item
		for ch := range h.waiters[k] {
			close(ch)
		}
		delete(h.waiters, k)
	}
}

// Since returns k's states newer than cursor, and the cursor to pass next
// time. With cursor 0 (a first poll) it returns who is typing now, without
// the "stopped" ones. A cursor from the future (another process, or a clock
// set back) counts as 0, so a client is never stuck behind it.
func (h *RTTypingHub) Since(k RTInboxHubKey, cursor proto.RTTypingCursor, now time.Time) ([]proto.RTTyping, proto.RTTypingCursor) {
	h.Lock()
	defer h.Unlock()

	since := uint64(cursor)
	if since > h.lastStamp {
		since = 0
	}
	box := h.inboxes[k]
	pruneTypingInbox(box, now)
	if box != nil && len(box) == 0 {
		delete(h.inboxes, k)
	}
	var ret []proto.RTTyping
	for ik, it := range box {
		if it.stamp <= since || (since == 0 && !it.on) {
			continue
		}
		var ttl time.Duration
		if it.on {
			ttl = it.until.Sub(now)
		}
		ret = append(ret, proto.RTTyping{
			Chid: ik.chid,
			Uid:  ik.uid,
			Ttl:  proto.ExportDurationMilli(ttl),
		})
	}
	return ret, proto.RTTypingCursor(h.lastStamp)
}

// Subscribe registers a waiter and returns its one-shot wake channel, closed
// by the next Publish that reaches k. Subscribe BEFORE calling Since, and
// always pair with Unsubscribe.
func (h *RTTypingHub) Subscribe(k RTInboxHubKey) chan struct{} {
	h.Lock()
	defer h.Unlock()
	ch := make(chan struct{})
	set := h.waiters[k]
	if set == nil {
		set = make(map[chan struct{}]bool)
		h.waiters[k] = set
	}
	set[ch] = true
	return ch
}

func (h *RTTypingHub) Unsubscribe(k RTInboxHubKey, ch chan struct{}) {
	h.Lock()
	defer h.Unlock()
	set := h.waiters[k]
	if set == nil {
		return
	}
	delete(set, ch)
	if len(set) == 0 {
		delete(h.waiters, k)
	}
}

// NumWaiters reports how many polls are subscribed on k. For tests.
func (h *RTTypingHub) NumWaiters(k RTInboxHubKey) int {
	h.Lock()
	defer h.Unlock()
	return len(h.waiters[k])
}

// Size reports how many sender and recipient entries the hub holds. For
// metrics and tests.
func (h *RTTypingHub) Size() (senders int, items int) {
	h.Lock()
	defer h.Unlock()
	for _, set := range h.senders {
		senders += len(set)
	}
	for _, box := range h.inboxes {
		items += len(box)
	}
	return senders, items
}

func pruneTypingInbox(box map[rtTypingItemKey]rtTypingItem, now time.Time) {
	for ik, it := range box {
		if !now.Before(it.until) {
			delete(box, ik)
		}
	}
}

// sweepLocked drops everything that ran out, at most once per
// rtTypingSweepEvery. It runs on the publishing path, so the hub needs no
// goroutine of its own; what the last burst of typing left behind stays
// until the next call, and is bounded by the limits above.
func (h *RTTypingHub) sweepLocked(now time.Time) {
	if now.Sub(h.lastSweep) < rtTypingSweepEvery && !now.Before(h.lastSweep) {
		return
	}
	h.lastSweep = now
	for k, set := range h.senders {
		for uid, s := range set {
			if !now.Before(s.until) {
				delete(set, uid)
			}
		}
		if len(set) == 0 {
			delete(h.senders, k)
		}
	}
	for k, box := range h.inboxes {
		pruneTypingInbox(box, now)
		if len(box) == 0 {
			delete(h.inboxes, k)
		}
	}
}
