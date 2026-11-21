// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// An unlinked file is unreadable at once, by name and by ID, whoever kept
// the ID: the server does not wait for the GC (kvStore assertLinked). The
// GC is not run in these tests.

func (f *gcFixture) requireUnreadableByID(t *testing.T, fid proto.FileID) {
	_, err := f.kvm.GetFileChunk(f.m, lcl.KVConfig{}, fid, 0)
	require.IsType(t, core.KVNoentError{}, err)
	_, err = f.cli.KvGetNode(f.m.Ctx(), rem.KvGetNodeArg{Id: fid.KVNodeID()})
	require.IsType(t, core.KVNoentError{}, err)
	// The raw chunk call, as a client that kept the ID and the key would
	// make it: refused too, at every offset.
	for _, off := range []proto.Offset{0, testChunkSize} {
		_, err = f.cli.KvGetEncryptedChunk(f.m.Ctx(), rem.KvGetEncryptedChunkArg{Id: fid, Offset: off})
		require.IsType(t, core.KVNoentError{}, err, "offset %d", off)
	}
}

func (f *gcFixture) requireReadableByID(t *testing.T, fid proto.FileID) {
	_, err := f.kvm.GetFileChunk(f.m, lcl.KVConfig{}, fid, 0)
	require.NoError(t, err)
	_, err = f.cli.KvGetNode(f.m.Ctx(), rem.KvGetNodeArg{Id: fid.KVNodeID()})
	require.NoError(t, err)
	_, err = f.cli.KvGetEncryptedChunk(f.m.Ctx(), rem.KvGetEncryptedChunkArg{Id: fid, Offset: testChunkSize})
	require.NoError(t, err)
}

// TestKVUnlinkedFileUnreadable: a large file reads by ID while it is linked
// and stops the moment its name is unlinked; its rows stay for the GC.
func TestKVUnlinkedFileUnreadable(t *testing.T) {
	f := newGCFixture(t)
	path := f.path("gone.bin")
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, path, 3*testChunkSize+5,
		lcl.KVConfig{MkdirP: true}, 0)
	fid := f.fileID(t, path)
	f.requireReadableByID(t, fid)
	readFile(t, f.kvm, f.m, path, buf)

	require.NoError(t, f.kvm.Unlink(f.m, lcl.KVConfig{}, path))
	f.requireUnreadableByID(t, fid)
	_, err := f.kvm.GetFile(f.m, lcl.KVConfig{}, path)
	require.IsType(t, core.KVNoentError{}, err)
	// Still there for the GC to collect and count.
	require.Equal(t, gcRow{"active", 0, 4}, f.row(t, fid))
}

// TestKVMovedFileReadable: a move links the file at its new name in the
// same write, so it never reads as unlinked.
func TestKVMovedFileReadable(t *testing.T) {
	f := newGCFixture(t)
	src, dst := f.path("from.bin"), f.path("to.bin")
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, src, 2*testChunkSize+9,
		lcl.KVConfig{MkdirP: true}, 0)
	fid := f.fileID(t, src)
	require.NoError(t, f.kvm.Mv(f.m, lcl.KVConfig{}, src, dst))
	f.requireReadableByID(t, fid)
	readFile(t, f.kvm, f.m, dst, buf)
	require.Equal(t, int64(1), f.row(t, fid).refcount)
}

// TestKVOverwrittenFileUnreadable: an overwrite unlinks the old file; its ID
// stops reading, the name reads the new one.
func TestKVOverwrittenFileUnreadable(t *testing.T) {
	f := newGCFixture(t)
	path := f.path("over.bin")
	cfg := lcl.KVConfig{MkdirP: true, OverwriteOk: true}
	writeRandomFileWithConfig(t, f.kvm, f.m, path, 2*testChunkSize+1, cfg, 0)
	old := f.fileID(t, path)
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, path, 2*testChunkSize+3, cfg, 0)
	fid := f.fileID(t, path)
	require.NotEqual(t, old, fid)
	f.requireUnreadableByID(t, old)
	f.requireReadableByID(t, fid)
	readFile(t, f.kvm, f.m, path, buf)
}

// TestKVUnlinkedSmallFileUnreadable: a small file (stored whole in its own
// row, not in chunks) is refused by node ID once unlinked, too.
func TestKVUnlinkedSmallFileUnreadable(t *testing.T) {
	f := newGCFixture(t)
	path := f.path("small.txt")
	res, err := f.kvm.PutFileFirst(f.m, lcl.KVConfig{MkdirP: true}, path, []byte("tiny"), true)
	require.NoError(t, err)
	typ, err := res.NodeID.Type()
	require.NoError(t, err)
	require.Equal(t, proto.KVNodeType_SmallFile, typ)

	got, err := f.cli.KvGetNode(f.m.Ctx(), rem.KvGetNodeArg{Id: res.NodeID})
	require.NoError(t, err)
	gt, err := got.GetT()
	require.NoError(t, err)
	require.Equal(t, proto.KVNodeType_SmallFile, gt)

	require.NoError(t, f.kvm.Unlink(f.m, lcl.KVConfig{}, path))
	_, err = f.cli.KvGetNode(f.m.Ctx(), rem.KvGetNodeArg{Id: res.NodeID})
	require.IsType(t, core.KVNoentError{}, err)

	// By name, too.
	_, err = f.kvm.GetFile(f.m, lcl.KVConfig{}, path)
	require.IsType(t, core.KVNoentError{}, err)
}

// TestKVDriftedRefcountStillReadable: a refcount of 0 alone does not make a
// file unreadable while a live name still links it (as the GC checks).
func TestKVDriftedRefcountStillReadable(t *testing.T) {
	f := newGCFixture(t)
	path := f.path("drift.bin")
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, path, 2*testChunkSize+7,
		lcl.KVConfig{MkdirP: true}, 0)
	fid := f.fileID(t, path)
	m := f.tew.MetaContext()
	err := m.ForAllShards(func(db *pgxpool.Conn) error {
		_, err := db.Exec(m.Ctx(), `UPDATE large_file SET refcount=0 WHERE file_id=$1`, fid.ExportToDB())
		return err
	})
	require.NoError(t, err)
	require.Equal(t, int64(0), f.row(t, fid).refcount)
	f.requireReadableByID(t, fid)
	readFile(t, f.kvm, f.m, path, buf)
}
