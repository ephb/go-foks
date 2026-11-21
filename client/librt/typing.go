// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"time"

	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

// SetTyping tells the channel's other readers that the caller is typing in
// it (on) or stopped. A hint: nothing is stored anywhere, nothing is queued
// when the server cannot be reached, and the server forgets the state by
// itself after about ten seconds, so a caller that keeps typing repeats the
// call every few seconds. The server rate-limits per user and channel and
// answers a call inside the limit with success.
//
// A server that predates rtSetTyping answers with rpc.MethodV2NotFoundError;
// the caller should then stop calling.
func (d *Minder) SetTyping(
	m MetaContext,
	team lcl.ConfigTeam,
	appID proto.RTAppID,
	channel lcl.RTChannelSpecifier,
	on bool,
) error {
	err := assertTeam(team)
	if err != nil {
		return err
	}
	rtp, err := d.base.GetParty(m.Base(), team)
	if err != nil {
		return err
	}
	ch, err := d.resolveChannel(m, rtp, appID, channel)
	if err != nil {
		return err
	}
	return d.SetTypingByID(m, ch.Id, on)
}

// SetTypingByID is SetTyping for a caller that already knows the channel's
// ID, sparing the channel listing.
func (d *Minder) SetTypingByID(
	m MetaContext,
	chid proto.RTChannelID,
	on bool,
) error {
	_, cli, err := d.clientLocal(m.Base(), d.au)
	if err != nil {
		return err
	}
	return cli.RtSetTyping(m.Ctx(), rem.RtSetTypingArg{
		Chid:   chid,
		Typing: on,
	})
}

// PollInboxTyping is PollInbox that also asks for other members' typing
// states. The poll returns early, with Bumped false, when one arrives; the
// result's Typing lists the states newer than typingSince (each with the
// channel's short ID, the user, and how much longer it holds; 0 = stopped)
// and TypingCursor is what to pass as typingSince next time (0 on the first
// call). A server that predates typing ignores the request and returns
// neither.
func (d *Minder) PollInboxTyping(
	m MetaContext,
	appID proto.RTAppID,
	since proto.RTInboxVersion,
	typingSince proto.RTTypingCursor,
	timeout time.Duration,
) (
	*proto.RTInboxPollRes,
	error,
) {
	_, cli, err := d.clientLocal(m.Base(), d.au)
	if err != nil {
		return nil, err
	}
	res, err := cli.RtPollInbox(m.Ctx(), rem.RTPollInboxArg{
		AppID:       appID,
		Since:       since,
		Timeout:     proto.ExportDurationMilli(timeout),
		Typing:      true,
		TypingSince: typingSince,
	})
	if err != nil {
		return nil, err
	}
	return &res, nil
}
