// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

import (
	"fmt"
	"sync"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Who wrote a large file.
//
// A KV store's names are encrypted and its roles are per team, not per
// member, so the server cannot tell an embedder whose a file is from what it
// stores about the name. It can record who uploaded the bytes: the user, in a
// user's own store, or the team member whose bearer token the upload used.
// large_file.writer (patch p2) holds that, as an encoded FQParty, from the
// first chunk on. kvFileInfo hands it to anyone who may read the file, so an
// application can check that a file it is told to delete is the caller's
// own (chat-foks: an attachment named in a message being deleted) without
// trusting what the message says. A file uploaded before p2 has no writer.
//
// The server enforces nothing with it: who may unlink a name is still the
// name's write role.

func encodeWriter(w proto.FQParty) ([]byte, error) {
	return core.EncodeToBytes(&w)
}

// writerCol remembers the shards (by database) that have
// large_file.writer.
var writerCol sync.Map

// ForgetWriterColumns makes hasWriterColumn look again, for a test that
// takes the column away.
func ForgetWriterColumns() {
	writerCol.Clear()
}

// hasWriterColumn reports whether the shard behind db has large_file.writer
// (patch p2). A server whose shards were not patched yet keeps uploading,
// without recording writers, instead of failing every upload. Only a yes is
// remembered: a shard is patched while the server runs, never unpatched.
func hasWriterColumn(m shared.MetaContext, db *pgxpool.Conn) (bool, error) {
	cfg := db.Conn().Config()
	key := fmt.Sprintf("%s:%d/%s", cfg.Host, cfg.Port, cfg.Database)
	if _, ok := writerCol.Load(key); ok {
		return true, nil
	}
	var ok bool
	err := db.QueryRow(
		m.Ctx(),
		`SELECT EXISTS(SELECT 1 FROM information_schema.columns
		  WHERE table_schema = current_schema()
		    AND table_name = 'large_file' AND column_name = 'writer')`,
	).Scan(&ok)
	if err != nil {
		return false, err
	}
	if ok {
		writerCol.Store(key, true)
	}
	return ok, nil
}

// getFileInfo answers kvFileInfo: authorized like a read of the file, and
// like one it says "no such file" for a file that is collected or that no
// name links (assertLinked), and "upload in progress" for one being
// uploaded.
func getFileInfo(
	m shared.MetaContext,
	db *pgxpool.Conn,
	pid proto.PartyID,
	role proto.Role,
	fid proto.FileID,
	withWriter bool,
) (
	*rem.KVFileInfo,
	error,
) {
	fileRole, st, refcount, err := loadLargeFileReadRole(m, db, pid, fid)
	if err != nil {
		return nil, err
	}
	err = assertAtOrAbove(role, fileRole, proto.KVOp_Read, proto.KVNodeType_File)
	if err != nil {
		return nil, err
	}
	switch st {
	case LargeFileStatusDead:
		return nil, core.KVNoentError{}
	case LargeFileStatusUploading:
		return nil, core.KVUploadInProgressError{}
	case LargeFileStatusActive:
		err = assertLinked(m, db, pid, fid.KVNodeID(), refcount)
		if err != nil {
			return nil, err
		}
	default:
		return nil, core.BadServerDataError("bad file status")
	}
	var ret rem.KVFileInfo
	if !withWriter {
		return &ret, nil
	}
	var raw []byte
	err = db.QueryRow(
		m.Ctx(),
		`SELECT writer FROM large_file
		 WHERE short_host_id=$1 AND short_party_id=$2 AND file_id=$3`,
		int(m.ShortHostID()), pid.Shorten().ExportToDB(), fid.ExportToDB(),
	).Scan(&raw)
	if err != nil {
		return nil, err
	}
	if raw == nil {
		return &ret, nil
	}
	var w proto.FQParty
	err = core.DecodeFromBytes(&w, raw)
	if err != nil {
		return nil, err
	}
	ret.Writer = &w
	return &ret, nil
}
