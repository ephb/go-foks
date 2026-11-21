// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package realtime

import (
	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
)

// SetChannelFlags sets the authenticated user's (m.UID()) own muted and/or
// hidden flag for a channel: the per-user preferences on their user_channels
// row that the inbox sync returns. An absent flag is left alone.
//
// Only the caller's row can be named, since the uid comes from the
// connection. The caller is authorized like a reader of the channel (as for
// rtReadThrough): someone who can no longer read it has no inbox row worth
// changing. A change bumps the caller's inbox version and stamps the row at
// it, as a read mark does, so the caller's other devices pick the flags up on
// their next sync, and their parked polls are woken. A call that changes
// nothing bumps nothing and wakes nobody.
func SetChannelFlags(
	m shared.MetaContext,
	arg rem.RtSetChannelFlagsArg,
) error {
	if arg.Muted == nil && arg.Hidden == nil {
		return core.BadArgsError("no channel flag to set")
	}
	rtdb, err := m.Db(shared.DbTypeRealTime)
	if err != nil {
		return err
	}
	defer rtdb.Release()
	userdb, err := m.Db(shared.DbTypeUsers)
	if err != nil {
		return err
	}
	defer userdb.Release()

	return shared.RetryTx2(m,
		rtdb,
		"realtime.SetChannelFlags",
		func(m shared.MetaContext, tx pgx.Tx) (func(shared.MetaContext), error) {
			// The marker loads and authorizes the channel and stamps the
			// caller's row exactly as a read mark does.
			r := readThroughMarker{
				arg:    rem.RTReadThroughArg{ChannelID: arg.Chid},
				reader: m.UID(),
				userdb: userdb,
				tx:     tx,
			}
			changed, err := setChannelFlagsTx(m, &r, arg)
			if err != nil {
				return nil, err
			}
			if !changed {
				return nil, nil
			}
			return func(m shared.MetaContext) {
				wakeInboxPollers(m, r.app, []proto.UID{r.reader})
			}, nil
		},
	)
}

func setChannelFlagsTx(
	m shared.MetaContext,
	r *readThroughMarker,
	arg rem.RtSetChannelFlagsArg,
) (bool, error) {
	err := r.loadChannel(m)
	if err != nil {
		return false, err
	}
	err = r.authorize(m)
	if err != nil {
		return false, err
	}

	// A NULL parameter keeps the column. The IS DISTINCT FROM guard makes a
	// repeated call a no-op, so it does not churn the inbox version.
	tag, err := r.tx.Exec(
		m.Ctx(),
		`UPDATE user_channels
		 SET muted = COALESCE($4, muted), hidden = COALESCE($5, hidden),
		     mtime = NOW()
		 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3
		   AND (muted IS DISTINCT FROM COALESCE($4, muted)
		     OR hidden IS DISTINCT FROM COALESCE($5, hidden))`,
		m.ShortHostID(),
		r.channelID(),
		r.reader.ExportToDB(),
		arg.Muted,
		arg.Hidden,
	)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		// Nothing to change, or no row: the user was never fanned into
		// this channel (it has had no message since they could read it).
		var one int
		err = r.tx.QueryRow(
			m.Ctx(),
			`SELECT 1 FROM user_channels
			 WHERE short_host_id=$1 AND channel_id=$2 AND uid=$3`,
			m.ShortHostID(),
			r.channelID(),
			r.reader.ExportToDB(),
		).Scan(&one)
		if err == pgx.ErrNoRows {
			return false, core.RowNotFoundError{}
		}
		return false, err
	}
	err = r.stampInbox(m)
	if err != nil {
		return false, err
	}
	return true, nil
}
