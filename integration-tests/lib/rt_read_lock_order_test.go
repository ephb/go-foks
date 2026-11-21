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

// A send locks the members' user_inbox rows and then their user_channels
// rows. A read mark must take its locks in the same order, or the two
// deadlock and Postgres aborts one of them.
//
// The test stands in for a send that has got as far as the inbox rows: it
// holds them in a transaction of its own, and lets a read mark run into them.
// The mark has to wait there without holding its user_channels row, so the
// "send" can still lock that row. A mark that wrote user_channels first would
// hold it by now, and a real send would be stuck in a deadlock.
func TestRTReadThroughLocksInboxFirst(t *testing.T) {
	sc := setupRTMutScene(t)
	_, err := sc.ada.Send(sc.adaM, sc.teamCfg(), proto.RTAppID_Chat, sc.spec(), []byte("one"))
	require.NoError(t, err)

	m := sc.tew.MetaContext()
	rtdb, err := m.Db(shared.DbTypeRealTime)
	require.NoError(t, err)
	defer rtdb.Release()
	tx, err := rtdb.Begin(m.Ctx())
	require.NoError(t, err)
	defer func() { _ = tx.Rollback(m.Ctx()) }()

	// The send's first step: every inbox row of the host is more than enough.
	_, err = tx.Exec(m.Ctx(),
		`SELECT 1 FROM user_inbox WHERE short_host_id=$1 FOR UPDATE`,
		m.ShortHostID())
	require.NoError(t, err)

	markErr := make(chan error, 1)
	go func() {
		markErr <- sc.mo.ReadThrough(sc.moM, sc.teamCfg(), proto.RTAppID_Chat, sc.spec(), 1)
	}()

	// Wait until the mark is stuck behind a lock: that is ours.
	waiting := false
	for i := 0; i < 500 && !waiting; i++ {
		var n int
		err = tx.QueryRow(m.Ctx(),
			`SELECT count(*) FROM pg_locks WHERE NOT granted`).Scan(&n)
		require.NoError(t, err)
		waiting = n > 0
		if !waiting {
			time.Sleep(20 * time.Millisecond)
		}
	}
	require.True(t, waiting, "the read mark never waited for the inbox row")

	// The send's second step. NOWAIT turns "would wait for the mark" (a
	// deadlock, with a real send) into an error at once.
	_, err = tx.Exec(m.Ctx(),
		`SELECT 1 FROM user_channels
		 WHERE short_host_id=$1 AND channel_id=$2
		 FOR UPDATE NOWAIT`,
		m.ShortHostID(), sc.pubID.Short().Int64())
	require.NoError(t, err, "the read mark holds its user_channels row while it waits for user_inbox")

	// The "send" is done; the mark goes through.
	require.NoError(t, tx.Commit(m.Ctx()))
	require.NoError(t, <-markErr)
}
