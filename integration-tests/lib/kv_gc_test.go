// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	kvStore "github.com/foks-proj/go-foks/server/kv-store"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// The GC tests run passes with the default graces (1 hour unlinked, 24
// hours uploading) and age the file under test by moving its mtime back,
// so that files other tests left behind are never old enough to be touched.

type gcFixture struct {
	tew  *TestEnvWrapper
	u    *TestUser
	m    libkv.MetaContext
	kvm  *libkv.Minder
	cli  rem.KVStoreClient
	root string
}

func newGCFixture(t *testing.T) *gcFixture {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	m := libkv.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
	// Cache-free, so every read goes to the server.
	kvm := libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
	root, err := core.RandomDomain()
	require.NoError(t, err)
	return &gcFixture{
		tew:  tew,
		u:    u,
		m:    m,
		kvm:  kvm,
		cli:  kvStoreClientForUser(t, tew, u),
		root: root,
	}
}

func (f *gcFixture) path(s string) proto.KVPath {
	return proto.KVPath("/" + f.root + "/" + s)
}

func (f *gcFixture) usage(t *testing.T) proto.KVUsageStatsChunked {
	u, err := f.cli.KvUsage(f.m.Ctx(), rem.KVAuth{})
	require.NoError(t, err)
	return u.Large
}

func (f *gcFixture) fileID(t *testing.T, path proto.KVPath) proto.FileID {
	gfr, err := f.kvm.GetFile(f.m, lcl.KVConfig{}, path)
	require.NoError(t, err)
	require.NotNil(t, gfr.Id)
	return *gfr.Id
}

// gc runs one pass with the default graces.
func (f *gcFixture) gc(t *testing.T) kvStore.GCStats {
	st, err := kvStore.CollectGarbage(
		f.tew.MetaContext(),
		kvStore.GCOptsFromConfig(shared.DefaultKVGCConfig()),
	)
	require.NoError(t, err)
	return st
}

// age moves a file's mtime back, as if it had been quiet for d.
func (f *gcFixture) age(t *testing.T, fid proto.FileID, d time.Duration) {
	m := f.tew.MetaContext()
	err := m.ForAllShards(func(db *pgxpool.Conn) error {
		_, err := db.Exec(m.Ctx(),
			`UPDATE large_file SET mtime = mtime - make_interval(secs => $2)
			 WHERE file_id=$1`,
			fid.ExportToDB(), d.Seconds(),
		)
		return err
	})
	require.NoError(t, err)
}

type gcRow struct {
	status   string
	refcount int64
	chunks   int64
}

func (f *gcFixture) row(t *testing.T, fid proto.FileID) gcRow {
	m := f.tew.MetaContext()
	var ret gcRow
	err := m.ForAllShards(func(db *pgxpool.Conn) error {
		var r gcRow
		err := db.QueryRow(m.Ctx(),
			`SELECT coalesce((SELECT status::text FROM large_file WHERE file_id=$1),''),
			        coalesce((SELECT refcount FROM large_file WHERE file_id=$1),-1),
			        (SELECT count(*) FROM large_file_chunk WHERE file_id=$1)`,
			fid.ExportToDB(),
		).Scan(&r.status, &r.refcount, &r.chunks)
		if err != nil {
			return err
		}
		if r.status != "" {
			ret.status, ret.refcount = r.status, r.refcount
		}
		ret.chunks += r.chunks
		return nil
	})
	require.NoError(t, err)
	return ret
}

// requireCollected checks that a file's chunks are gone and that reading it
// by file ID, as another device would, fails with "no such file".
func (f *gcFixture) requireCollected(t *testing.T, fid proto.FileID) {
	r := f.row(t, fid)
	require.Equal(t, "dead", r.status)
	require.Zero(t, r.chunks)

	_, err := f.kvm.GetFileChunk(f.m, lcl.KVConfig{}, fid, 0)
	require.Error(t, err)
	require.IsType(t, core.KVNoentError{}, err)

	nid := fid.KVNodeID()
	_, err = f.cli.KvGetNode(f.m.Ctx(), rem.KvGetNodeArg{Id: nid})
	require.Error(t, err)
	require.IsType(t, core.KVNoentError{}, err)
}

// TestKVGCUnlinkedFile: a file stays while it is linked and for the grace
// after its unlink, then the GC deletes its chunks, gives the space back
// and reads by file ID say "no such file".
func TestKVGCUnlinkedFile(t *testing.T) {
	f := newGCFixture(t)
	before := f.usage(t)

	path := f.path("a.bin")
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, path, 3*testChunkSize+77,
		lcl.KVConfig{MkdirP: true}, 0)
	fid := f.fileID(t, path)
	during := f.usage(t)
	require.Equal(t, before.Base.Num+1, during.Base.Num)
	require.Equal(t, before.NumChunks+4, during.NumChunks)
	require.Greater(t, during.Base.Sum, before.Base.Sum+proto.Size(len(buf)))

	// Linked, even when old: kept.
	f.age(t, fid, 48*time.Hour)
	f.gc(t)
	require.Equal(t, gcRow{"active", 1, 4}, f.row(t, fid))
	readFile(t, f.kvm, f.m, path, buf)

	require.NoError(t, f.kvm.Unlink(f.m, lcl.KVConfig{}, path))

	// Unlinked a moment ago: kept (the grace covers upload-then-link).
	f.gc(t)
	require.Equal(t, gcRow{"active", 0, 4}, f.row(t, fid))
	_, err := f.kvm.GetFileChunk(f.m, lcl.KVConfig{}, fid, 0)
	require.NoError(t, err)

	// Unlinked for longer than the grace: collected.
	f.age(t, fid, 2*time.Hour)
	st := f.gc(t)
	require.GreaterOrEqual(t, st.Unlinked, 1)
	require.GreaterOrEqual(t, st.Chunks, 4)
	f.requireCollected(t, fid)
	require.Equal(t, before, f.usage(t))

	// A second pass has nothing more to do with it.
	f.age(t, fid, 48*time.Hour)
	f.gc(t)
	require.Equal(t, before, f.usage(t))
	require.Equal(t, gcRow{"dead", 0, 0}, f.row(t, fid))
}

// TestKVGCReplacedFile: overwriting a file unlinks the old one, which the GC
// then collects; the new one is untouched.
func TestKVGCReplacedFile(t *testing.T) {
	f := newGCFixture(t)
	before := f.usage(t)

	path := f.path("b.bin")
	cfg := lcl.KVConfig{MkdirP: true, OverwriteOk: true}
	writeRandomFileWithConfig(t, f.kvm, f.m, path, 2*testChunkSize+5, cfg, 0)
	oldID := f.fileID(t, path)
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, path, 2*testChunkSize+9, cfg, 0)
	newID := f.fileID(t, path)
	require.NotEqual(t, oldID, newID)
	withNew := f.usage(t)

	f.age(t, oldID, 2*time.Hour)
	f.age(t, newID, 2*time.Hour)
	f.gc(t)
	f.requireCollected(t, oldID)
	require.Equal(t, gcRow{"active", 1, 3}, f.row(t, newID))
	readFile(t, f.kvm, f.m, path, buf)

	after := f.usage(t)
	require.Equal(t, before.Base.Num+1, after.Base.Num)
	require.Equal(t, withNew.Base.Num-1, after.Base.Num)
	require.Equal(t, withNew.NumChunks-3, after.NumChunks)
	require.Less(t, after.Base.Sum, withNew.Base.Sum)
}

// TestKVGCAbandonedUpload: an upload that stops half way is collected once
// it has been quiet for the upload grace, even though libkv already linked
// it. The dirent then reads as "no such file", the upload can't go on, and
// the dirent can still be removed.
func TestKVGCAbandonedUpload(t *testing.T) {
	f := newGCFixture(t)
	before := f.usage(t)

	cfg := lcl.KVConfig{MkdirP: true}
	start := func(path proto.KVPath) proto.FileID {
		buf := make([]byte, 2*testChunkSize)
		require.NoError(t, core.RandomFill(buf))
		res, err := f.kvm.PutFileFirst(f.m, cfg, path, buf[:testChunkSize], false)
		require.NoError(t, err)
		fid, err := res.NodeID.ToFileID()
		require.NoError(t, err)
		err = f.kvm.PutFileChunk(f.m, cfg, *fid, buf[testChunkSize:], testChunkSize, false)
		require.NoError(t, err)
		require.Equal(t, gcRow{"uploading", 1, 2}, f.row(t, *fid))
		return *fid
	}
	pathA, pathB := f.path("c.bin"), f.path("c2.bin")
	fidA, fidB := start(pathA), start(pathB)
	require.Equal(t, before.NumChunks+4, f.usage(t).NumChunks)

	// Quiet for less than the upload grace: kept.
	f.age(t, fidA, 23*time.Hour)
	f.age(t, fidB, 23*time.Hour)
	f.gc(t)
	require.Equal(t, gcRow{"uploading", 1, 2}, f.row(t, fidA))
	require.Equal(t, gcRow{"uploading", 1, 2}, f.row(t, fidB))

	// Quiet for longer: collected.
	f.age(t, fidA, 2*time.Hour)
	f.age(t, fidB, 2*time.Hour)
	st := f.gc(t)
	require.GreaterOrEqual(t, st.Uploads, 2)
	f.requireCollected(t, fidA)
	f.requireCollected(t, fidB)
	require.Equal(t, before, f.usage(t))

	// By name it reads as "no such file".
	_, err := f.kvm.GetFile(f.m, lcl.KVConfig{}, pathA)
	require.Error(t, err)
	require.IsType(t, core.KVNoentError{}, err)

	// The upload can't go on.
	var tail [64]byte
	err = f.kvm.PutFileChunk(f.m, cfg, fidA, tail[:], 2*testChunkSize, true)
	require.Error(t, err)
	require.IsType(t, core.UploadError(""), err)

	// The names are still usable: remove one, overwrite the other.
	require.NoError(t, f.kvm.Unlink(f.m, lcl.KVConfig{}, pathA))
	_, err = f.kvm.GetFile(f.m, lcl.KVConfig{}, pathA)
	require.IsType(t, core.KVNoentError{}, err)
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, pathB, testChunkSize+3,
		lcl.KVConfig{OverwriteOk: true}, 0)
	readFile(t, f.kvm, f.m, pathB, buf)
	require.Equal(t, before.Base.Num+1, f.usage(t).Base.Num)
	require.Equal(t, gcRow{"dead", 0, 0}, f.row(t, fidB))
}

// TestKVGCActiveUploadKept: each chunk counts as a sign of life, so an
// upload that started long ago but is still sending is not collected.
func TestKVGCActiveUploadKept(t *testing.T) {
	f := newGCFixture(t)
	path := f.path("d.bin")
	cfg := lcl.KVConfig{MkdirP: true}
	buf := make([]byte, 2*testChunkSize+10)
	require.NoError(t, core.RandomFill(buf))
	res, err := f.kvm.PutFileFirst(f.m, cfg, path, buf[:testChunkSize], false)
	require.NoError(t, err)
	fidp, err := res.NodeID.ToFileID()
	require.NoError(t, err)
	fid := *fidp

	f.age(t, fid, 30*time.Hour)
	err = f.kvm.PutFileChunk(f.m, cfg, fid, buf[testChunkSize:2*testChunkSize], testChunkSize, false)
	require.NoError(t, err)
	f.gc(t)
	require.Equal(t, gcRow{"uploading", 1, 2}, f.row(t, fid))

	err = f.kvm.PutFileChunk(f.m, cfg, fid, buf[2*testChunkSize:], 2*testChunkSize, true)
	require.NoError(t, err)
	readFile(t, f.kvm, f.m, path, buf)
}

// TestKVFileChunkReplay: a chunk sent again with the same bytes (a retry
// after a lost reply) succeeds without a second charge, including the final
// chunk after the file was finalized; different bytes at a stored offset
// are refused.
func TestKVFileChunkReplay(t *testing.T) {
	f := newGCFixture(t)
	ctx := f.m.Ctx()
	before := f.usage(t)

	var nid proto.KVNodeID
	nid[0] = byte(proto.KVNodeType_File)
	require.NoError(t, core.RandomFill(nid[1:]))
	fidp, err := nid.ToFileID()
	require.NoError(t, err)
	fid := *fidp

	// 4096 bytes of padded plaintext + 3 bytes of padding overhead + the
	// secretbox tag: each full chunk moves the offset by 4096. The server
	// checks sizes and offsets, not contents.
	const full = 4096 + 3 + 16
	mk := func(n int) proto.NaclCiphertext {
		b := make([]byte, n)
		require.NoError(t, core.RandomFill(b))
		return proto.NaclCiphertext(b)
	}
	c0, c1, c2 := mk(full), mk(full), mk(100)

	err = f.cli.KvFileUploadInit(ctx, rem.KvFileUploadInitArg{
		FileID: fid,
		Md: proto.LargeFileMetadata{
			Rg:   proto.RoleAndGen{Role: proto.OwnerRole},
			Vers: 1,
		},
		Chunk: proto.UploadChunk{Data: c0, Offset: 0},
	})
	require.NoError(t, err)

	chunk := func(data proto.NaclCiphertext, off proto.Offset, final bool) error {
		arg := rem.KvFileUploadChunkArg{
			FileID: fid,
			Chunk:  proto.UploadChunk{Data: data, Offset: off},
		}
		if final {
			arg.Chunk.Final = &proto.UploadFinal{Sz: proto.Size(2*full + 100)}
		}
		return f.cli.KvFileUploadChunk(ctx, arg)
	}

	require.NoError(t, chunk(c1, 4096, false))
	once := f.usage(t)
	require.Equal(t, before.NumChunks+2, once.NumChunks)

	// The same chunk again: fine, and not charged twice.
	require.NoError(t, chunk(c1, 4096, false))
	require.Equal(t, once, f.usage(t))

	// Different bytes at the same offset: refused.
	err = chunk(mk(full), 4096, false)
	require.Error(t, err)
	require.IsType(t, core.KVRaceError(""), err)

	// The final chunk, then the final chunk again after the file is done.
	require.NoError(t, chunk(c2, 8192, true))
	require.Equal(t, gcRow{"active", 0, 3}, f.row(t, fid))
	done := f.usage(t)
	require.NoError(t, chunk(c2, 8192, true))
	require.Equal(t, done, f.usage(t))

	// But no other chunk once the file is done.
	err = chunk(c1, 4096, false)
	require.Error(t, err)
	require.IsType(t, core.UploadError(""), err)

	// Never linked: once past the grace the GC takes it back.
	f.age(t, fid, 2*time.Hour)
	f.gc(t)
	require.Equal(t, gcRow{"dead", 0, 0}, f.row(t, fid))
	require.Equal(t, before, f.usage(t))

	// And a chunk for it now is refused.
	err = chunk(c2, 8192, true)
	require.Error(t, err)
	require.IsType(t, core.UploadError(""), err)
}
