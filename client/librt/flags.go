// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package librt

import (
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

// SetChannelFlags sets the caller's own muted and/or hidden flag for a
// channel on the server; nil leaves a flag as it is. The flags come back in
// the inbox (RTInboxRowView.Muted and Hidden) after the next SyncInbox, on
// this device and on the user's others. Nothing is queued when the server
// cannot be reached.
//
// A server that predates rtSetChannelFlags answers with
// rpc.MethodV2NotFoundError.
func (d *Minder) SetChannelFlags(
	m MetaContext,
	team lcl.ConfigTeam,
	appID proto.RTAppID,
	channel lcl.RTChannelSpecifier,
	muted *bool,
	hidden *bool,
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
	return d.SetChannelFlagsByID(m, ch.Id, muted, hidden)
}

// SetChannelFlagsByID is SetChannelFlags for a caller that already knows the
// channel's ID, sparing the channel listing.
func (d *Minder) SetChannelFlagsByID(
	m MetaContext,
	chid proto.RTChannelID,
	muted *bool,
	hidden *bool,
) error {
	_, cli, err := d.clientLocal(m.Base(), d.au)
	if err != nil {
		return err
	}
	return cli.RtSetChannelFlags(m.Ctx(), rem.RtSetChannelFlagsArg{
		Chid:   chid,
		Muted:  muted,
		Hidden: hidden,
	})
}
