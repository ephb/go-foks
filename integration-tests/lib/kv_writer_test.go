// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	kvStore "github.com/foks-proj/go-foks/server/kv-store"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func (u *TestUser) fqParty() proto.FQParty {
	return proto.FQParty{Party: u.uid.ToPartyID(), Host: u.host}
}

// TestKVFileInfoWriter: in a team's store, kvFileInfo tells any member who
// may read a file which member uploaded it, and says "no such file" once the
// file is unlinked. UnlinkNode leaves a name alone that links another file.
func TestKVFileInfoWriter(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	alice := tew.NewTestUser(t)
	bob := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	tm.makeChanges(t, tew.MetaContext(), owner,
		[]proto.MemberRole{
			alice.toMemberRole(t, proto.DefaultRole, tm.hepks),
			bob.toMemberRole(t, proto.DefaultRole, tm.hepks),
		}, nil)

	actingAs := team.WrapNamed(proto.FQTeamParsed{
		Team: proto.NewParsedTeamWithFalse(tm.FQTeam(t).Team),
	})
	minder := func(u *TestUser) (libkv.MetaContext, *libkv.Minder) {
		m := libkv.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
		return m, libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
	}
	cfg := lcl.KVConfig{
		ActingAs: actingAs,
		Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
	}
	plain := lcl.KVConfig{ActingAs: actingAs}
	mOwner, kvOwner := minder(owner)
	mkdirs := cfg
	mkdirs.MkdirP = true
	_, err := kvOwner.Mkdir(mOwner, mkdirs, "/att")
	require.NoError(t, err)

	mA, kvA := minder(alice)
	mB, kvB := minder(bob)
	upload := func(m libkv.MetaContext, kvm *libkv.Minder, path proto.KVPath) proto.FileID {
		writeRandomFileWithConfig(t, kvm, m, path, 2*testChunkSize+11, cfg, 0)
		gfr, err := kvm.GetFile(m, plain, path)
		require.NoError(t, err)
		require.NotNil(t, gfr.Id)
		return *gfr.Id
	}
	fa := upload(mA, kvA, "/att/alice.bin")
	fb := upload(mB, kvB, "/att/bob.bin")

	writer := func(m libkv.MetaContext, kvm *libkv.Minder, fid proto.FileID) *proto.FQParty {
		info, err := kvm.FileInfo(m, plain, fid)
		require.NoError(t, err)
		return info.Writer
	}
	// Each sees who wrote each file.
	for _, who := range []struct {
		m   libkv.MetaContext
		kvm *libkv.Minder
	}{{mA, kvA}, {mB, kvB}, {mOwner, kvOwner}} {
		w := writer(who.m, who.kvm, fa)
		require.NotNil(t, w)
		require.Equal(t, alice.fqParty(), *w)
		w = writer(who.m, who.kvm, fb)
		require.NotNil(t, w)
		require.Equal(t, bob.fqParty(), *w)
	}

	// UnlinkNode of alice's name with bob's file in hand does nothing.
	err = kvB.UnlinkNode(mB, plain, "/att/alice.bin", fb.KVNodeID())
	require.IsType(t, core.KVNoentError{}, err)
	require.Equal(t, alice.fqParty(), *writer(mB, kvB, fa))
	_, err = kvB.GetFileChunk(mB, plain, fa, 0)
	require.NoError(t, err)

	// With the right file it unlinks, and the file is gone for everyone.
	require.NoError(t, kvA.UnlinkNode(mA, plain, "/att/alice.bin", fa.KVNodeID()))
	for _, who := range []struct {
		m   libkv.MetaContext
		kvm *libkv.Minder
	}{{mA, kvA}, {mB, kvB}} {
		_, err = who.kvm.FileInfo(who.m, plain, fa)
		require.IsType(t, core.KVNoentError{}, err)
		_, err = who.kvm.GetFileChunk(who.m, plain, fa, 0)
		require.IsType(t, core.KVNoentError{}, err)
	}
	// Again: the name is gone.
	err = kvA.UnlinkNode(mA, plain, "/att/alice.bin", fa.KVNodeID())
	require.IsType(t, core.KVNoentError{}, err)
}

// TestKVStorePatchP2 upgrades a KV shard from p1 to p2 (large_file.writer).
// Before the patch, uploads go on without a writer; after it, new files
// have one and old ones still have none.
func TestKVStorePatchP2(t *testing.T) {
	f := newGCFixture(t)
	m := f.tew.MetaContext()
	defer kvStore.ForgetWriterColumns()

	var nShards int
	err := m.ForAllShards(func(db *pgxpool.Conn) error {
		nShards++
		return nil
	})
	require.NoError(t, err)

	// A fresh install has the column and the patch.
	old := f.path("before.bin")
	writeRandomFileWithConfig(t, f.kvm, f.m, old, 2*testChunkSize, lcl.KVConfig{MkdirP: true}, 0)
	info, err := f.kvm.FileInfo(f.m, lcl.KVConfig{}, f.fileID(t, old))
	require.NoError(t, err)
	require.NotNil(t, info.Writer)
	require.Equal(t, f.u.fqParty(), *info.Writer)

	// Back to a server with p1 only.
	err = m.ForAllShards(func(db *pgxpool.Conn) error {
		for _, q := range []string{
			`ALTER TABLE large_file DROP COLUMN writer`,
			`DELETE FROM schema_patches WHERE id = 2`,
		} {
			_, err := db.Exec(m.Ctx(), q)
			if err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	kvStore.ForgetWriterColumns()

	// Uploads still work, and record no writer.
	unpatched := f.path("unpatched.bin")
	buf := writeRandomFileWithConfig(t, f.kvm, f.m, unpatched, 2*testChunkSize, lcl.KVConfig{}, 0)
	readFile(t, f.kvm, f.m, unpatched, buf)
	fu := f.fileID(t, unpatched)
	info, err = f.kvm.FileInfo(f.m, lcl.KVConfig{}, fu)
	require.NoError(t, err)
	require.Nil(t, info.Writer)

	eng := &shared.PatchDBEng{
		Which: shared.DbTypeKVStore,
		ConfirmHook: func(m shared.MetaContext, ps *shared.PatchSummary) error {
			require.Len(t, ps.Desc(), nShards)
			return nil
		},
	}
	require.NoError(t, eng.Run(m))

	// The file from before the patch still has no writer; a new one has.
	info, err = f.kvm.FileInfo(f.m, lcl.KVConfig{}, fu)
	require.NoError(t, err)
	require.Nil(t, info.Writer)
	after := f.path("after.bin")
	writeRandomFileWithConfig(t, f.kvm, f.m, after, 2*testChunkSize, lcl.KVConfig{}, 0)
	info, err = f.kvm.FileInfo(f.m, lcl.KVConfig{}, f.fileID(t, after))
	require.NoError(t, err)
	require.NotNil(t, info.Writer)
	require.Equal(t, f.u.fqParty(), *info.Writer)

	err = m.ForAllShards(func(db *pgxpool.Conn) error {
		var n int
		err := db.QueryRow(m.Ctx(), `SELECT COUNT(*) FROM schema_patches WHERE id = 2`).Scan(&n)
		if err != nil {
			return err
		}
		if n != 1 {
			return core.NotFoundError("schema_patches row 2")
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, core.NotFoundError("no patches to apply"), eng.Run(m))
}
