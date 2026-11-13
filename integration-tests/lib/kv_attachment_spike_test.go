//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// Spike S1 for chat-foks attachments (docs/notes/10-media-design.md, §6;
// results in docs/notes/12-media-spikes.md). It drives libkv.Minder
// in-process, the way chatd would, and moves one big file through a named
// team's KV store and through an ad-hoc (DM) team's.
//
// Off by default: set KV_SPIKE=1 (and KV_SPIKE_MIB, default 100) to run it.
// It prints timings and Postgres sizes with t.Logf; run with -v.

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// spikeChunk is one KV chunk as chatd would upload it. Every non-final chunk
// must be a power of two: KV pads each chunk (lib/kv/crypto.go PaddedLen) and
// the server works the plaintext offsets back out of the padded sizes
// (server/kv-store/file.go checkChunks), so a 1 MiB + 16 chunk counts as 2 MiB
// and the upload fails at the end with "chunks didn't line up". chatd must
// seal 1 MiB - 16 bytes of plaintext so that each sealed chunk is 1 MiB.
const spikeChunk = 1 << 20

type spikeDBStats struct {
	dbBytes     int64
	chunkBytes  int64 // pg_total_relation_size(large_file_chunk), TOAST included
	chunkRows   int64
	chunkData   int64 // sum(length(data))
	largeFiles  int64
	dbBytesDesc string
}

func spikeDB(t *testing.T, tew *TestEnvWrapper) spikeDBStats {
	m := tew.MetaContext()
	var ret spikeDBStats
	err := m.ForAllShards(func(conn *pgxpool.Conn) error {
		var s spikeDBStats
		err := conn.QueryRow(m.Ctx(),
			`SELECT pg_database_size(current_database()),
			        pg_total_relation_size('large_file_chunk'),
			        (SELECT count(*) FROM large_file_chunk),
			        (SELECT coalesce(sum(length(data)),0) FROM large_file_chunk),
			        (SELECT count(*) FROM large_file)`,
		).Scan(&s.dbBytes, &s.chunkBytes, &s.chunkRows, &s.chunkData, &s.largeFiles)
		if err != nil {
			return err
		}
		ret.dbBytes += s.dbBytes
		ret.chunkBytes += s.chunkBytes
		ret.chunkRows += s.chunkRows
		ret.chunkData += s.chunkData
		ret.largeFiles += s.largeFiles
		return nil
	})
	require.NoError(t, err)
	return ret
}

// spikeFileRows reports what the server still holds for one file.
func spikeFileRows(t *testing.T, tew *TestEnvWrapper, fid proto.FileID) (status string, refcount int64, chunks int64) {
	m := tew.MetaContext()
	err := m.ForAllShards(func(conn *pgxpool.Conn) error {
		var st string
		var rc, n int64
		err := conn.QueryRow(m.Ctx(),
			`SELECT coalesce((SELECT status::text FROM large_file WHERE file_id=$1),''),
			        coalesce((SELECT refcount FROM large_file WHERE file_id=$1),-1),
			        (SELECT count(*) FROM large_file_chunk WHERE file_id=$1)`,
			fid.ExportToDB(),
		).Scan(&st, &rc, &n)
		if err != nil {
			return err
		}
		if st != "" {
			status, refcount = st, rc
		}
		chunks += n
		return nil
	})
	require.NoError(t, err)
	return status, refcount, chunks
}

func mib(n int64) string { return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20)) }

func rate(n int64, d time.Duration) string {
	return fmt.Sprintf("%.1f MiB/s", float64(n)/(1<<20)/d.Seconds())
}

func spikeMinder(tew *TestEnvWrapper, t *testing.T, u *TestUser) (libkv.MetaContext, *libkv.Minder) {
	m := libkv.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
	// Cache-free, so reads measure the server and not the client's chunk cache.
	kvm := libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
	return m, kvm
}

func spikeRandHex(t *testing.T, n int) string {
	b := make([]byte, n)
	require.NoError(t, core.RandomFill(b))
	return hex.EncodeToString(b)
}

// spikeUpload writes buf to path in spikeChunk pieces, chatd-style.
func spikeUpload(
	t *testing.T,
	m libkv.MetaContext,
	kvm *libkv.Minder,
	cfg lcl.KVConfig,
	path proto.KVPath,
	buf []byte,
) (proto.FileID, time.Duration) {
	fid, d, err := spikeUploadChunked(m, kvm, cfg, path, buf, spikeChunk)
	require.NoError(t, err)
	return fid, d
}

func spikeUploadChunked(
	m libkv.MetaContext,
	kvm *libkv.Minder,
	cfg lcl.KVConfig,
	path proto.KVPath,
	buf []byte,
	chunk int,
) (proto.FileID, time.Duration, error) {
	start := time.Now()
	var fid proto.FileID
	for off := 0; off < len(buf); off += chunk {
		end := min(off+chunk, len(buf))
		last := end == len(buf)
		if off == 0 {
			res, err := kvm.PutFileFirst(m, cfg, path, buf[off:end], last)
			if err != nil {
				return fid, 0, fmt.Errorf("first chunk: %w", err)
			}
			f, err := res.NodeID.ToFileID()
			if err != nil {
				return fid, 0, err
			}
			fid = *f
		} else {
			err := kvm.PutFileChunk(m, cfg, fid, buf[off:end], proto.Offset(off), last)
			if err != nil {
				return fid, 0, fmt.Errorf("chunk at %d (final=%v): %w", off, last, err)
			}
		}
	}
	return fid, time.Since(start), nil
}

// spikeReadAll reads the file sequentially by path, then by id and offset.
func spikeReadAll(
	t *testing.T,
	m libkv.MetaContext,
	kvm *libkv.Minder,
	cfg lcl.KVConfig,
	path proto.KVPath,
	want []byte,
) time.Duration {
	start := time.Now()
	var got bytes.Buffer
	gfr, err := kvm.GetFile(m, cfg, path)
	require.NoError(t, err)
	require.NotNil(t, gfr.Id)
	got.Write(gfr.Chunk.Chunk)
	final := gfr.Chunk.Final
	for !final {
		c, err := kvm.GetFileChunk(m, cfg, *gfr.Id, proto.Offset(got.Len()))
		require.NoError(t, err)
		require.NotEmpty(t, c.Chunk)
		got.Write(c.Chunk)
		final = c.Final
	}
	d := time.Since(start)
	require.Equal(t, len(want), got.Len())
	require.True(t, bytes.Equal(want, got.Bytes()))
	return d
}

// spikeSeeks reads n random offsets by file id and checks each chunk.
func spikeSeeks(
	t *testing.T,
	m libkv.MetaContext,
	kvm *libkv.Minder,
	cfg lcl.KVConfig,
	fid proto.FileID,
	want []byte,
	n int,
) []time.Duration {
	rng := rand.New(rand.NewSource(42))
	var ds []time.Duration
	for range n {
		off := rng.Intn(len(want))
		start := time.Now()
		c, err := kvm.GetFileChunk(m, cfg, fid, proto.Offset(off))
		ds = append(ds, time.Since(start))
		require.NoError(t, err)
		// The rest of the chunk that holds off, from off on.
		cs := (off / spikeChunk) * spikeChunk
		require.Equal(t, want[off:min(cs+spikeChunk, len(want))], []byte(c.Chunk))
	}
	slices.Sort(ds)
	return ds
}

func pct(ds []time.Duration, p float64) time.Duration {
	i := int(float64(len(ds)-1) * p)
	return ds[i]
}

func spikeSize(t *testing.T) int {
	n := 100
	if s := os.Getenv("KV_SPIKE_MIB"); s != "" {
		v, err := strconv.Atoi(s)
		require.NoError(t, err)
		n = v
	}
	return n * (1 << 20)
}

func TestKVAttachmentSpike(t *testing.T) {
	if os.Getenv("KV_SPIKE") == "" {
		t.Skip("spike S1; set KV_SPIKE=1 to run")
	}
	sz := spikeSize(t)
	t.Run("named", func(t *testing.T) { testKVSpikeNamed(t, sz) })
	t.Run("adhoc", func(t *testing.T) { testKVSpikeAdHoc(t, sz) })
}

func testKVSpikeNamed(t *testing.T, sz int) {
	tew := testEnvBeta(t)

	owner := tew.NewTestUser(t)
	member := tew.NewTestUser(t)  // uploads
	member2 := tew.NewTestUser(t) // reads
	lowly := tew.NewTestUser(t)   // a member below the file's read role
	outsider := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	tm := tew.makeTeamForOwner(t, owner)
	lowRole := proto.NewRoleWithMember(proto.VizLevel(-10))
	tm.makeChanges(
		t, tew.MetaContext(), owner,
		[]proto.MemberRole{
			member.toMemberRole(t, proto.DefaultRole, tm.hepks),
			member2.toMemberRole(t, proto.DefaultRole, tm.hepks),
			lowly.toMemberRole(t, lowRole, tm.hepks),
		}, nil,
	)
	tew.DirectDoubleMerklePokeInTest(t)

	actingAs := team.WrapNamed(proto.FQTeamParsed{
		Team: proto.NewParsedTeamWithFalse(tm.FQTeam(t).Team),
	})
	// Channel roles: read and write at the default member role.
	cfg := lcl.KVConfig{
		ActingAs: actingAs,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		MkdirP:   true,
	}
	readCfg := lcl.KVConfig{ActingAs: actingAs}

	chid := spikeRandHex(t, 16)
	attid := spikeRandHex(t, 16)
	dir := proto.KVPath("/chat-foks/att/" + chid + "/" + attid)
	path := dir + "/file"

	// 1. Root permission path: a plain member can't make /chat-foks (the
	// team root is admin-write).
	mMem, kvMem := spikeMinder(tew, t, member)
	_, err := kvMem.Mkdir(mMem, cfg, dir)
	t.Logf("named: member mkdir %s before an admin made /chat-foks: err=%v", dir, err)
	require.Error(t, err)

	// The owner makes /chat-foks with member write once.
	mOwn, kvOwn := spikeMinder(tew, t, owner)
	_, err = kvOwn.Mkdir(mOwn, cfg, "/chat-foks")
	require.NoError(t, err)

	// Now the member makes the rest of the path.
	start := time.Now()
	_, err = kvMem.Mkdir(mMem, cfg, dir)
	require.NoError(t, err)
	t.Logf("named: member mkdir -p %s: %v", dir, time.Since(start))

	// 0. Chunks that aren't a power of two: accepted one by one, refused at
	// the final chunk.
	odd := make([]byte, 3*(1<<20+16))
	require.NoError(t, core.RandomFill(odd))
	_, _, err = spikeUploadChunked(mMem, kvMem, lcl.KVConfig{
		ActingAs: actingAs,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	}, dir+"/odd", odd, 1<<20+16)
	t.Logf("named: upload in 1 MiB + 16 chunks: err=%v", err)
	require.Error(t, err)

	buf := make([]byte, sz)
	require.NoError(t, core.RandomFill(buf))

	before := spikeDB(t, tew)
	fid, dUp := spikeUpload(t, mMem, kvMem, lcl.KVConfig{
		ActingAs: actingAs,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	}, path, buf)
	after := spikeDB(t, tew)
	t.Logf("named: upload %s in %d-byte chunks: %v (%s)", mib(int64(sz)), spikeChunk, dUp, rate(int64(sz), dUp))
	t.Logf("named: postgres db %d -> %d (+%s), large_file_chunk %d -> %d (+%s), rows +%d, sum(length(data)) +%s, large_file rows +%d; growth/payload = %.3f",
		before.dbBytes, after.dbBytes, mib(after.dbBytes-before.dbBytes),
		before.chunkBytes, after.chunkBytes, mib(after.chunkBytes-before.chunkBytes),
		after.chunkRows-before.chunkRows, mib(after.chunkData-before.chunkData),
		after.largeFiles-before.largeFiles,
		float64(after.dbBytes-before.dbBytes)/float64(sz))

	// 2. Another member reads it (sequential, then seeks by id).
	mR, kvR := spikeMinder(tew, t, member2)
	dSeq := spikeReadAll(t, mR, kvR, readCfg, path, buf)
	t.Logf("named: member2 sequential read: %v (%s)", dSeq, rate(int64(sz), dSeq))
	ds := spikeSeeks(t, mR, kvR, readCfg, fid, buf, 20)
	t.Logf("named: member2 20 random seeks by file id: min %v median %v p95 %v max %v",
		ds[0], pct(ds, 0.5), pct(ds, 0.95), ds[len(ds)-1])

	// 3. A member below the file's read role is refused, by path and by id.
	mL, kvL := spikeMinder(tew, t, lowly)
	_, err = kvL.GetFile(mL, readCfg, path)
	t.Logf("named: lowly (member/-10) GetFile: err=%v", err)
	require.Error(t, err)
	_, err = kvL.GetFileChunk(mL, readCfg, fid, 0)
	t.Logf("named: lowly GetFileChunk by id: err=%v", err)
	require.Error(t, err)

	// 4. A non-member is refused.
	mO, kvO := spikeMinder(tew, t, outsider)
	_, err = kvO.GetFile(mO, readCfg, path)
	t.Logf("named: outsider GetFile: err=%v", err)
	require.Error(t, err)
	_, err = kvO.GetFileChunk(mO, readCfg, fid, 0)
	t.Logf("named: outsider GetFileChunk by id: err=%v", err)
	require.Error(t, err)

	// 5. Usage, then unlink, then what's left on the server.
	u1, err := kvMem.GetUsage(mMem, readCfg)
	require.NoError(t, err)
	t.Logf("named: usage before unlink: large num=%d sum=%d chunks=%d; small num=%d sum=%d",
		u1.Large.Base.Num, u1.Large.Base.Sum, u1.Large.NumChunks, u1.Small.Num, u1.Small.Sum)
	err = kvMem.Unlink(mMem, readCfg, path)
	require.NoError(t, err)
	u2, err := kvMem.GetUsage(mMem, readCfg)
	require.NoError(t, err)
	st, rc, nch := spikeFileRows(t, tew, fid)
	t.Logf("named: after unlink: usage large num=%d sum=%d chunks=%d; large_file status=%q refcount=%d, chunk rows=%d",
		u2.Large.Base.Num, u2.Large.Base.Sum, u2.Large.NumChunks, st, rc, nch)
	_, err = kvR.GetFileChunk(mR, readCfg, fid, 0)
	t.Logf("named: member2 GetFileChunk by id after unlink: err=%v", err)
	afterUnlink := spikeDB(t, tew)
	t.Logf("named: postgres db after unlink: %d (%+d vs. after upload)", afterUnlink.dbBytes, afterUnlink.dbBytes-after.dbBytes)
}

func testKVSpikeAdHoc(t *testing.T, sz int) {
	f := createAdHocTeamForTest(t, 1)
	tew := f.tew
	bob := f.others[0]

	actingAs := lcl.NewConfigTeamWithAdhoc(proto.FQAdHocTeamParsed{
		Team: proto.NewAdHocTeamParsedWithId(f.tid.EntityID()),
	})
	cfg := lcl.KVConfig{
		ActingAs: actingAs,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		MkdirP:   true,
	}
	readCfg := lcl.KVConfig{ActingAs: actingAs}
	path := proto.KVPath("/chat-foks/att/" + spikeRandHex(t, 16) + "/" + spikeRandHex(t, 16) + "/file")

	mA, kvA := spikeMinder(tew, t, f.alice)
	dir := path[:len(path)-len("/file")]
	start := time.Now()
	_, err := kvA.Mkdir(mA, cfg, dir)
	t.Logf("adhoc: alice mkdir -p %s (as an owner, no admin step): %v err=%v", dir, time.Since(start), err)
	require.NoError(t, err)

	buf := make([]byte, sz)
	require.NoError(t, core.RandomFill(buf))
	before := spikeDB(t, tew)
	fid, dUp := spikeUpload(t, mA, kvA, lcl.KVConfig{
		ActingAs: actingAs,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	}, path, buf)
	after := spikeDB(t, tew)
	t.Logf("adhoc: upload %s: %v (%s); postgres db +%s, large_file_chunk +%s",
		mib(int64(sz)), dUp, rate(int64(sz), dUp),
		mib(after.dbBytes-before.dbBytes), mib(after.chunkBytes-before.chunkBytes))

	mB, kvB := spikeMinder(tew, t, bob)
	dSeq := spikeReadAll(t, mB, kvB, readCfg, path, buf)
	t.Logf("adhoc: bob sequential read: %v (%s)", dSeq, rate(int64(sz), dSeq))
	ds := spikeSeeks(t, mB, kvB, readCfg, fid, buf, 20)
	t.Logf("adhoc: bob 20 random seeks: min %v median %v p95 %v max %v",
		ds[0], pct(ds, 0.5), pct(ds, 0.95), ds[len(ds)-1])

	// A user on the same vhost who isn't in the DM.
	carol := tew.NewTestUserAtVHost(t, f.vhost)
	tew.DirectDoubleMerklePokeInTest(t)
	mC, kvC := spikeMinder(tew, t, carol)
	_, err = kvC.GetFileChunk(mC, readCfg, fid, 0)
	t.Logf("adhoc: carol (not in the DM) GetFileChunk by id: err=%v", err)
	require.Error(t, err)
}
