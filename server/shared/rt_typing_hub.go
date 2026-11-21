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
//
// What is bounded, and by what:
//
//   - How often one user wakes the others: once per RTTypingMinInterval per
//     channel for "typing", once per "typing" for "stopped", and
//     RTTypingMaxPerSender times per RTTypingMinInterval over all channels.
//   - How often a recipient's poll returns because of typing alone: once per
//     RTTypingMinReturnInterval, whoever types (RTTypingHub.ReturnDelay).
//   - What a call costs the server: one channel row, one membership check
//     and, when admitted, a roster read of at most RTTypingMaxAudience+2
//     rows; none of that roster read for RTTypingTooLargeTTL once a channel
//     was found too large.
//   - Memory: per channel at most RTTypingMaxPerChannel people typing, one
//     budget entry per sender, per recipient at most RTTypingMaxPerInbox
//     states and one poll-return time, one mark per too-large channel; all
//     of it swept once it ran out.
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
	// RTTypingMaxPerSender caps the fan-outs ("typing" or "stopped") one
	// user gets in any RTTypingMinInterval, over all channels together.
	// Without it the per-channel interval alone would let a member of many
	// channels wake the other members' polls as often as it has channels.
	RTTypingMaxPerSender = 4
	// RTTypingMinReturnInterval is the least time between two results of a
	// recipient's polls that carry nothing but typing states. States that
	// arrive inside it are handed over together when it ends.
	RTTypingMinReturnInterval = time.Second
	// RTTypingMaxPerInbox caps the states held for one recipient.
	RTTypingMaxPerInbox = 64
	// RTTypingMaxAudience is the largest number of readers a typing state is
	// fanned out to. A channel with more gets none, and neither does any
	// channel of a team with more than RTTypingMaxAudience+1 members: the
	// server reads no more of a roster than that.
	RTTypingMaxAudience = 256
	// RTTypingTooLargeTTL is how long the hub remembers that a channel's
	// audience was too large, so that typing into it costs no roster read.
	RTTypingTooLargeTTL = time.Minute

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

// rtTypingBudgetKey is one sender across all channels of a host.
type rtTypingBudgetKey struct {
	host core.ShortHostID
	uid  proto.UID
}

// rtTypingBudget holds the times of a sender's last admitted fan-outs.
type rtTypingBudget struct {
	at   [RTTypingMaxPerSender]time.Time
	next int
}

// has reports whether another fan-out fits: the oldest of the last
// RTTypingMaxPerSender lies a whole interval back (or the clock went back).
func (b *rtTypingBudget) has(now time.Time) bool {
	if b == nil {
		return true
	}
	oldest := b.at[b.next]
	return oldest.IsZero() || now.Sub(oldest) >= RTTypingMinInterval || now.Before(oldest)
}

// idle reports whether nothing in the budget counts any more.
func (b *rtTypingBudget) idle(now time.Time) bool {
	for _, t := range b.at {
		if !t.IsZero() && now.Sub(t) < RTTypingMinInterval && !now.Before(t) {
			return false
		}
	}
	return true
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
	budgets   map[rtTypingBudgetKey]*rtTypingBudget
	tooLarge  map[RTTypingChannelKey]time.Time // until when Admit refuses
	maxAud    int
	returned  map[RTInboxHubKey]time.Time // the last typing-only poll result
	inboxes   map[RTInboxHubKey]map[rtTypingItemKey]rtTypingItem
	waiters   map[RTInboxHubKey]map[chan struct{}]bool
	lastStamp uint64
	lastSweep time.Time
}

func NewRTTypingHub() *RTTypingHub {
	return &RTTypingHub{
		senders:  make(map[RTTypingChannelKey]map[proto.UID]*rtTypingSender),
		budgets:  make(map[rtTypingBudgetKey]*rtTypingBudget),
		tooLarge: make(map[RTTypingChannelKey]time.Time),
		maxAud:   RTTypingMaxAudience,
		returned: make(map[RTInboxHubKey]time.Time),
		inboxes:  make(map[RTInboxHubKey]map[rtTypingItemKey]rtTypingItem),
		waiters:  make(map[RTInboxHubKey]map[chan struct{}]bool),
	}
}

// Admit is the rate limit: it reports whether uid's new state in the channel
// is to be fanned out, and records it if so. Call it only for a caller
// already authorized for the channel, so that an entry can only be made for
// a channel the caller may write to.
//
// "Typing" is admitted at most once per RTTypingMinInterval per user and
// channel, and not for a new typer while RTTypingMaxPerChannel others are
// typing (those who stopped or ran out do not count). "Stopped" is admitted
// once, and only while the user's "typing" is still live. Either kind is
// admitted at most RTTypingMaxPerSender times per RTTypingMinInterval per
// user, over all channels. Nothing is admitted for a channel marked too
// large (MarkTooLarge) while the mark holds.
func (h *RTTypingHub) Admit(k RTTypingChannelKey, uid proto.UID, on bool, now time.Time) bool {
	h.Lock()
	defer h.Unlock()
	h.sweepLocked(now)

	if until, ok := h.tooLarge[k]; ok {
		if now.Before(until) && !now.Before(until.Add(-RTTypingTooLargeTTL)) {
			return false
		}
		delete(h.tooLarge, k)
	}

	bk := rtTypingBudgetKey{host: k.HostID, uid: uid}
	budget := h.budgets[bk]
	spend := func() {
		if budget == nil {
			budget = &rtTypingBudget{}
			h.budgets[bk] = budget
		}
		budget.at[budget.next] = now
		budget.next = (budget.next + 1) % len(budget.at)
	}

	set := h.senders[k]
	s := set[uid]
	if !on {
		if s == nil || !s.on || !now.Before(s.until) {
			return false
		}
		// Out of budget: the state stays "typing" and runs out by itself.
		if !budget.has(now) {
			return false
		}
		s.on = false
		spend()
		return true
	}
	if s != nil && now.Sub(s.lastOn) < RTTypingMinInterval {
		return false
	}
	if !budget.has(now) {
		return false
	}
	if s == nil {
		live := 0
		for _, o := range set {
			if o.on && now.Before(o.until) {
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
	spend()
	return true
}

// MaxAudience is the largest audience a typing state is fanned out to.
func (h *RTTypingHub) MaxAudience() int {
	h.Lock()
	defer h.Unlock()
	return h.maxAud
}

// SetMaxAudienceForTest changes the largest audience and returns the old one.
func (h *RTTypingHub) SetMaxAudienceForTest(n int) int {
	h.Lock()
	defer h.Unlock()
	old := h.maxAud
	h.maxAud = n
	return old
}

// MarkTooLarge records that the channel's audience is larger than the
// fan-out allows. For RTTypingTooLargeTTL from now, Admit refuses every state
// for the channel, so the caller reads the roster at most once in that time.
// One entry per channel that someone with the right to send typed into; the
// sweep drops it when it ran out.
func (h *RTTypingHub) MarkTooLarge(k RTTypingChannelKey, now time.Time) {
	h.Lock()
	defer h.Unlock()
	h.tooLarge[k] = now.Add(RTTypingTooLargeTTL)
}

// ReturnDelay is how long a poll of k has to hold a result that carries
// nothing but typing states: what is left of RTTypingMinReturnInterval since
// the last such result, or zero. However often the states change, a
// recipient's poll loop then turns at most once per interval because of them.
func (h *RTTypingHub) ReturnDelay(k RTInboxHubKey, now time.Time) time.Duration {
	h.Lock()
	defer h.Unlock()
	last, ok := h.returned[k]
	if !ok || now.Before(last) {
		return 0
	}
	if d := RTTypingMinReturnInterval - now.Sub(last); d > 0 {
		return d
	}
	return 0
}

// NoteReturn records that a poll of k answered with typing states only.
func (h *RTTypingHub) NoteReturn(k RTInboxHubKey, now time.Time) {
	h.Lock()
	defer h.Unlock()
	h.sweepLocked(now)
	h.returned[k] = now
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

// sizeLimits reports how many budget and poll-return entries the hub holds.
// For tests.
func (h *RTTypingHub) sizeLimits() (budgets int, returned int) {
	h.Lock()
	defer h.Unlock()
	return len(h.budgets), len(h.returned)
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
	for k, until := range h.tooLarge {
		if !now.Before(until) || now.Before(until.Add(-RTTypingTooLargeTTL)) {
			delete(h.tooLarge, k)
		}
	}
	for k, b := range h.budgets {
		if b.idle(now) {
			delete(h.budgets, k)
		}
	}
	for k, t := range h.returned {
		if now.Sub(t) >= RTTypingMinReturnInterval || now.Before(t) {
			delete(h.returned, k)
		}
	}
}
