// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"errors"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// A delete (the unlink of a file's last name) must stay a delete: nobody
// links the file again by its ID. And a member can't give another member's
// file a name of their own, which the owner's delete would not remove.

func requireKVPermError(t *testing.T, err error) {
	require.Error(t, err)
	var pe core.PermissionError
	require.True(t, errors.As(err, &pe), "want a PermissionError, got %T %v", err, err)
}

func TestKVRelinkInTeam(t *testing.T) {
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
	mO, kvO := minder(owner)
	mkdirs := cfg
	mkdirs.MkdirP = true
	_, err := kvO.Mkdir(mO, mkdirs, "/att")
	require.NoError(t, err)
	mA, kvA := minder(alice)
	mB, kvB := minder(bob)

	buf := writeRandomFileWithConfig(t, kvA, mA, "/att/a.bin", 2*testChunkSize+11, cfg, 0)
	gfr, err := kvA.GetFile(mA, plain, "/att/a.bin")
	require.NoError(t, err)
	fa := *gfr.Id

	// bob can't give alice's file a name of his own, nor move it.
	_, err = kvB.LinkNode(mB, cfg, "/att/bobs.bin", fa.KVNodeID())
	requireKVPermError(t, err)
	err = kvB.Mv(mB, plain, "/att/a.bin", "/att/moved-by-bob.bin")
	requireKVPermError(t, err)
	readFileWithConfig(t, kvA, mA, "/att/a.bin", buf, plain, 0)

	// An admin can; and alice can move her own file.
	_, err = kvO.LinkNode(mO, cfg, "/att/owners.bin", fa.KVNodeID())
	require.NoError(t, err)
	require.NoError(t, kvO.Unlink(mO, plain, "/att/owners.bin"))
	require.NoError(t, kvA.Mv(mA, plain, "/att/a.bin", "/att/a2.bin"))
	readFileWithConfig(t, kvB, mB, "/att/a2.bin", buf, plain, 0)

	// alice deletes it. Nobody else links it again by ID: not bob, and not
	// an admin either (it was deleted, not just unnamed for a moment).
	require.NoError(t, kvA.Unlink(mA, plain, "/att/a2.bin"))
	_, err = kvO.LinkNode(mO, cfg, "/att/back.bin", fa.KVNodeID())
	require.IsType(t, core.KVNoentError{}, err)
	_, err = kvB.LinkNode(mB, cfg, "/att/back.bin", fa.KVNodeID())
	require.Error(t, err)
	_, err = kvB.GetFileChunk(mB, plain, fa, 0)
	require.IsType(t, core.KVNoentError{}, err)
}

// In one's own store: a small file (no recorded writer) that was unlinked
// can't be linked again by its node ID; a move still works.
func TestKVRelinkSmallFile(t *testing.T) {
	f := newGCFixture(t)
	path := f.path("small.txt")
	res, err := f.kvm.PutFileFirst(f.m, lcl.KVConfig{MkdirP: true}, path, []byte("tiny"), true)
	require.NoError(t, err)
	moved := f.path("small2.txt")
	require.NoError(t, f.kvm.Mv(f.m, lcl.KVConfig{}, path, moved))
	got, err := f.kvm.GetFile(f.m, lcl.KVConfig{}, moved)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.NoError(t, f.kvm.Unlink(f.m, lcl.KVConfig{}, moved))
	_, err = f.kvm.LinkNode(f.m, lcl.KVConfig{}, f.path("again.txt"), res.NodeID)
	require.IsType(t, core.KVNoentError{}, err)
}
