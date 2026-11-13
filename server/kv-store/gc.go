// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

import (
	"time"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/infra"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Garbage collection of large files.
//
// A large file's chunks stay in large_file_chunk, and count against the
// party's usage, until something removes them. Nothing did: unlinking a file
// only dropped its refcount, so the bytes stayed (and stayed readable by file
// ID), and an upload that stopped half way kept its rows forever. The GC
// collects two kinds of file:
//
//   - unlinked: status 'active', refcount 0, untouched for UnlinkedGrace.
//     fileRef bumps mtime on every refcount change, so this is "unlinked for
//     that long". The grace also covers the gap between finishing an upload
//     and linking the file, during which the refcount is 0 too.
//   - abandoned: status 'uploading', no chunk for UploadGrace. Linked or not:
//     libkv links a file after its first chunk, so most stalled uploads have
//     a dirent pointing at them, and that dirent then reads as "no such
//     file".
//
// Collecting a file deletes its chunks, marks the large_file row 'dead' (a
// tombstone, so the file ID is never reused and reads say KVNoentError) and
// takes the chunks off the party's (and the vhost's) usage. Each file is its
// own transaction, which locks the large_file row and re-checks the
// conditions, so a concurrent link, chunk upload or second collector either
// goes first and the file is skipped, or waits and then sees the tombstone.
// Replicas can run it at the same time.

// GCStats reports one pass.
type GCStats struct {
	Unlinked int   // finished files nothing linked to
	Uploads  int   // abandoned uploads
	Chunks   int   // chunk rows deleted
	Bytes    int64 // chunk bytes deleted
}

func (s *GCStats) add(o GCStats) {
	s.Unlinked += o.Unlinked
	s.Uploads += o.Uploads
	s.Chunks += o.Chunks
	s.Bytes += o.Bytes
}

// GCOpts are the knobs of one pass; see shared.KVGCConfig.
type GCOpts struct {
	UnlinkedGrace time.Duration
	UploadGrace   time.Duration
	BatchSize     int
}

func GCOptsFromConfig(c shared.KVGCConfig) GCOpts {
	return GCOpts{
		UnlinkedGrace: c.UnlinkedGrace,
		UploadGrace:   c.UploadGrace,
		BatchSize:     c.BatchSize,
	}
}

// CollectGarbage runs one pass over every KV shard and every host on it.
func CollectGarbage(m shared.MetaContext, opts GCOpts) (GCStats, error) {
	var tot GCStats
	if opts.BatchSize <= 0 {
		opts.BatchSize = shared.DefaultKVGCConfig().BatchSize
	}
	err := m.ForAllShards(func(db *pgxpool.Conn) error {
		st, err := collectShard(m, db, opts)
		tot.add(st)
		return err
	})
	return tot, err
}

type gcCandidate struct {
	shid  core.ShortHostID
	spid  proto.ShortPartyID
	fid   proto.FileID
	isUpl bool
}

func secs(d time.Duration) float64 { return d.Seconds() }

// loadGCBatch picks up to n candidates, oldest first. Each kind has its own
// partial index (patch foks_kv_store p1), so neither scan walks the
// tombstones of files already collected.
func loadGCBatch(
	m shared.MetaContext,
	db *pgxpool.Conn,
	opts GCOpts,
	skip map[proto.FileID]bool,
) ([]gcCandidate, error) {
	rows, err := db.Query(
		m.Ctx(),
		`(SELECT short_host_id, short_party_id, file_id, false
		  FROM large_file
		  WHERE refcount = 0 AND status = 'active'
		    AND mtime < NOW() - make_interval(secs => $1)
		  ORDER BY mtime ASC LIMIT $3)
		UNION ALL
		(SELECT short_host_id, short_party_id, file_id, true
		  FROM large_file
		  WHERE status = 'uploading'
		    AND mtime < NOW() - make_interval(secs => $2)
		  ORDER BY mtime ASC LIMIT $3)`,
		secs(opts.UnlinkedGrace),
		secs(opts.UploadGrace),
		opts.BatchSize+len(skip),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ret []gcCandidate
	for rows.Next() {
		var shid int
		var spid, fid []byte
		var c gcCandidate
		err = rows.Scan(&shid, &spid, &fid, &c.isUpl)
		if err != nil {
			return nil, err
		}
		if len(spid) != len(c.spid) {
			return nil, core.BadServerDataError("short party id")
		}
		copy(c.spid[:], spid)
		err = c.fid.ImportFromDB(fid)
		if err != nil {
			return nil, err
		}
		if skip[c.fid] {
			continue
		}
		c.shid = core.ShortHostID(shid)
		ret = append(ret, c)
	}
	return ret, rows.Err()
}

func collectShard(m shared.MetaContext, db *pgxpool.Conn, opts GCOpts) (GCStats, error) {
	var tot GCStats
	// Files we looked at and left (skipped or failed), so the next batch
	// doesn't pick them again. Collected files drop out of the query.
	left := make(map[proto.FileID]bool)
	for {
		batch, err := loadGCBatch(m, db, opts, left)
		if err != nil {
			return tot, err
		}
		if len(batch) == 0 {
			return tot, nil
		}
		for _, c := range batch {
			st, err := collectFile(m, db, opts, c)
			if err != nil {
				// One bad row must not stop the GC for everyone else.
				m.Warnw("kv gc", "host", c.shid, "file", c.fid.KVNodeID(), "err", err)
			}
			if err != nil || st.Unlinked+st.Uploads == 0 {
				left[c.fid] = true
				continue
			}
			tot.add(st)
		}
	}
}

func collectFile(
	m shared.MetaContext,
	db *pgxpool.Conn,
	opts GCOpts,
	c gcCandidate,
) (GCStats, error) {
	var ret GCStats
	m, err := m.WithShortHostID(c.shid)
	if err != nil {
		return ret, err
	}
	err = shared.RetryTx(m, db, "kvGC", func(m shared.MetaContext, tx pgx.Tx) error {
		ret = GCStats{}
		return collectFileTx(m, tx, opts, c, &ret)
	})
	if err != nil {
		return GCStats{}, err
	}
	return ret, nil
}

func collectFileTx(
	m shared.MetaContext,
	tx pgx.Tx,
	opts GCOpts,
	c gcCandidate,
	st *GCStats,
) error {
	shid := int(c.shid)
	spid := c.spid.ExportToDB()
	fid := c.fid.ExportToDB()

	// Lock the row and check again under the lock: the candidate query ran
	// outside any transaction. SKIP LOCKED leaves a file that someone is
	// linking or uploading to right now for a later pass.
	var status string
	var refcount int
	var stillDue bool
	err := tx.QueryRow(
		m.Ctx(),
		`SELECT status, refcount,
		    CASE WHEN status = 'uploading'
		      THEN mtime < NOW() - make_interval(secs => $5)
		      ELSE mtime < NOW() - make_interval(secs => $4)
		    END
		 FROM large_file
		 WHERE short_host_id=$1 AND short_party_id=$2 AND file_id=$3
		 FOR UPDATE SKIP LOCKED`,
		shid, spid, fid,
		secs(opts.UnlinkedGrace), secs(opts.UploadGrace),
	).Scan(&status, &refcount, &stillDue)
	if err == pgx.ErrNoRows {
		return nil // gone, or locked by someone else
	}
	if err != nil {
		return err
	}
	st0, err := ParseLargeFileStatus(status)
	if err != nil {
		return err
	}
	switch {
	case !stillDue:
		return nil
	case st0 == LargeFileStatusUploading:
	case st0 == LargeFileStatusActive && refcount == 0:
		// Belt and braces: never collect a file a live dirent still points
		// at, whatever the refcount says.
		var linked bool
		nid := c.fid.KVNodeID()
		err = tx.QueryRow(
			m.Ctx(),
			`SELECT EXISTS(SELECT 1 FROM dirent
			  WHERE short_host_id=$1 AND short_party_id=$2
			    AND active AND value=$3)`,
			shid, spid, nid.ExportToDB(),
		).Scan(&linked)
		if err != nil {
			return err
		}
		if linked {
			m.Warnw("kv gc: refcount 0 but a live dirent links the file; leaving it",
				"host", c.shid, "file", nid)
			return nil
		}
	default:
		return nil
	}

	var nChunks int
	var nBytes int64
	var hasFirst bool
	err = tx.QueryRow(
		m.Ctx(),
		`SELECT COUNT(*), COALESCE(SUM(OCTET_LENGTH(data)), 0),
		     COALESCE(BOOL_OR(chunk_offset = 0), false)
		 FROM large_file_chunk
		 WHERE short_host_id=$1 AND short_party_id=$2 AND file_id=$3`,
		shid, spid, fid,
	).Scan(&nChunks, &nBytes, &hasFirst)
	if err != nil {
		return err
	}
	_, err = tx.Exec(
		m.Ctx(),
		`DELETE FROM large_file_chunk
		 WHERE short_host_id=$1 AND short_party_id=$2 AND file_id=$3`,
		shid, spid, fid,
	)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(
		m.Ctx(),
		`UPDATE large_file SET status='dead', mtime=NOW()
		 WHERE short_host_id=$1 AND short_party_id=$2 AND file_id=$3`,
		shid, spid, fid,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return core.UpdateError("large_file")
	}
	err = usageDecLarge(m, tx, c.spid, nChunks, nBytes, hasFirst)
	if err != nil {
		return err
	}
	if st0 == LargeFileStatusUploading {
		st.Uploads = 1
	} else {
		st.Unlinked = 1
	}
	st.Chunks = nChunks
	st.Bytes = nBytes
	return nil
}

// usageDecLarge undoes what usageCheckAndInc charged for a large file's
// chunks: one file (charged with its first chunk), the chunk count and their
// bytes. In the teams scope that went to the party's row and to the vhost's;
// in the vhost scope only to the vhost's. It pokes the quota checker, so a
// party that was over quota is looked at again.
func usageDecLarge(
	m shared.MetaContext,
	tx pgx.Tx,
	spid proto.ShortPartyID,
	nChunks int,
	nBytes int64,
	hasFirst bool,
) error {
	if nChunks == 0 {
		return nil
	}
	numLarge := core.Sel(hasFirst, 1, 0)
	scope, err := quotaScope(m, tx)
	if err != nil {
		return err
	}

	if scope == infra.QuotaScope_Teams {
		pid, err := partyIDFromShort(m, tx, spid)
		if err != nil {
			return err
		}
		if pid != nil {
			_, err = tx.Exec(
				m.Ctx(),
				`UPDATE usage SET
				   num_large = GREATEST(num_large - $3, 0),
				   num_large_chunks = GREATEST(num_large_chunks - $4, 0),
				   sum_large = GREATEST(sum_large - $5, 0)
				 WHERE short_host_id=$1 AND party_id=$2`,
				int(m.ShortHostID()), pid.ExportToDB(), numLarge, nChunks, nBytes,
			)
			if err != nil {
				return err
			}
			err = pokeQuotaChecker(m, tx, *pid)
			if err != nil {
				return err
			}
		}
	}

	_, err = tx.Exec(
		m.Ctx(),
		`UPDATE usage_vhost SET
		   num_large = GREATEST(num_large - $2, 0),
		   num_large_chunks = GREATEST(num_large_chunks - $3, 0),
		   sum_large = GREATEST(sum_large - $4, 0)
		 WHERE short_host_id=$1`,
		int(m.ShortHostID()), numLarge, nChunks, nBytes,
	)
	if err != nil {
		return err
	}
	if scope == infra.QuotaScope_VHost {
		err = pokeQuotaCheckerVhost(m, tx)
		if err != nil {
			return err
		}
	}
	return nil
}

// partyIDFromShort finds the full party ID behind a short one. The KV tables
// key on the short ID (the first 17 bytes), but usage keys on the full one,
// and a party that was charged for a chunk has a usage row. Returns nil when
// there is none.
func partyIDFromShort(
	m shared.MetaContext,
	tx pgx.Tx,
	spid proto.ShortPartyID,
) (*proto.PartyID, error) {
	rows, err := tx.Query(
		m.Ctx(),
		`SELECT party_id FROM usage
		 WHERE short_host_id=$1 AND substring(party_id from 1 for $3) = $2`,
		int(m.ShortHostID()), spid.ExportToDB(), len(spid),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ret *proto.PartyID
	for rows.Next() {
		var raw []byte
		err = rows.Scan(&raw)
		if err != nil {
			return nil, err
		}
		if ret != nil {
			return nil, core.BadServerDataError("short party id is not unique in usage")
		}
		var pid proto.PartyID
		err = pid.ImportFromDB(raw)
		if err != nil {
			return nil, err
		}
		ret = &pid
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return ret, nil
}

// gcLoop runs a pass at startup and then every interval, until the server
// stops.
func gcLoop(m shared.MetaContext, cfg shared.KVGCConfig) {
	m = m.WithLogTag("kvgc")
	opts := GCOptsFromConfig(cfg)
	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		st, err := CollectGarbage(m, opts)
		if err != nil {
			m.Warnw("kv gc pass", "err", err)
		}
		if st.Unlinked+st.Uploads > 0 {
			m.Infow("kv gc pass", "unlinked", st.Unlinked, "uploads", st.Uploads,
				"chunks", st.Chunks, "bytes", st.Bytes)
		}
		select {
		case <-tick.C:
		case <-m.Ctx().Done():
			return
		}
	}
}
