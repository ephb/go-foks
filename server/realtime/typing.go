// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package realtime

import (
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

// typingChannel is what SetTyping needs to know about a channel.
type typingChannel struct {
	team       proto.TeamID
	writeRole  proto.Role
	readRole   proto.Role
	app        proto.RTAppID
	archivedAt *time.Time
}

func loadChannelForTyping(
	m shared.MetaContext,
	db shared.Querier,
	channelID int64,
) (
	*typingChannel,
	error,
) {
	var ret typingChannel
	var teamRaw []byte
	var wrt, wvl, rrt, rvl int
	var appRaw string
	err := db.QueryRow(
		m.Ctx(),
		`SELECT parent_team_id, write_role_type, write_role_viz_level,
		        read_role_type, read_role_viz_level, app_id, archived_at
		 FROM channels
		 WHERE short_host_id=$1 AND channel_id=$2`,
		m.ShortHostID(),
		channelID,
	).Scan(&teamRaw, &wrt, &wvl, &rrt, &rvl, &appRaw, &ret.archivedAt)
	if err == pgx.ErrNoRows {
		return nil, core.RowNotFoundError{}
	}
	if err != nil {
		return nil, err
	}
	err = ret.team.ImportFromDB(teamRaw)
	if err != nil {
		return nil, err
	}
	err = ret.writeRole.ImportFromDB(wrt, wvl)
	if err != nil {
		return nil, err
	}
	err = ret.readRole.ImportFromDB(rrt, rvl)
	if err != nil {
		return nil, err
	}
	err = ret.app.ImportFromDB(appRaw)
	if err != nil {
		return nil, err
	}
	return &ret, nil
}

// SetTyping tells the channel's other readers that the authenticated user is
// typing in it, or stopped. Nothing is written: the state goes to the
// in-process RTTypingHub, which hands it to the readers' parked rtPollInbox
// calls and forgets it after shared.RTTypingTTL.
//
// The caller is authorized exactly as for rtSend -- the channel's write role
// in the parent team, and the channel not archived -- and only then counted
// against the rate limit, so the limiter can only ever hold entries for
// channels the caller may write to. A call inside the limit returns nil and
// does nothing. The audience is read from the team's current roster (members
// at or above the channel's read role), not from user_channels rows, which
// outlive a removal from the team.
func SetTyping(m shared.MetaContext, arg rem.RtSetTypingArg) error {
	chid := arg.Chid.Short()
	ch, err := readChannelForTyping(m, chid)
	if err != nil {
		return err
	}

	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer userdb.Release()

	role, err := AuthorizeUserForTeam(m, userdb, ch.team)
	if err != nil {
		return err
	}
	writeRole, err := core.ImportRole(ch.writeRole)
	if err != nil {
		return err
	}
	if role.LessThan(*writeRole) {
		return core.PermissionError("user role too low to send into channel")
	}
	// After authorize, as in rtSend: a caller with no right to send learns
	// nothing about the channel's state.
	if ch.archivedAt != nil {
		return archivedBlocks(ch.archivedAt)
	}

	hub := m.G().RTTypingHub()
	now := m.G().Clock().Now()
	key := shared.RTTypingChannelKey{HostID: m.ShortHostID(), Chid: chid}
	if !hub.Admit(key, m.UID(), arg.Typing, now) {
		return nil
	}

	readRole, err := core.ImportRole(ch.readRole)
	if err != nil {
		return err
	}
	members, err := allActiveTeamMembers(m, userdb, ch.team)
	if err != nil {
		return err
	}
	recipients := make([]proto.UID, 0, len(members))
	for uid, r := range members {
		if uid == m.UID() || r.LessThan(*readRole) {
			continue
		}
		recipients = append(recipients, uid)
	}
	if len(recipients) == 0 || len(recipients) > shared.RTTypingMaxAudience {
		return nil
	}
	hub.Publish(m.ShortHostID(), ch.app, chid, m.UID(), arg.Typing, recipients, now)
	return nil
}

// readChannelForTyping reads the channel row, holding the pooled realtime
// connection only for that one query.
func readChannelForTyping(
	m shared.MetaContext,
	chid proto.RTChannelIDShort,
) (
	*typingChannel,
	error,
) {
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return nil, err
	}
	defer rtdb.Release()
	return loadChannelForTyping(m, rtdb, chid.Int64())
}
