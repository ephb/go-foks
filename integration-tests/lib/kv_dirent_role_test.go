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
	"github.com/stretchr/testify/require"
)

// A name in a directory carries its own write role, and changing or removing
// the name needs that role. The server did not hold the role of a new name
// against its maker's: a member could put a name with write role "owner" into
// a directory members write, and no admin could ever remove or replace it.
func TestKVDirentWriteRoleNotAboveCaller(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	admin := tew.NewTestUser(t)
	member := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)
	tm.makeChanges(t, tew.MetaContext(), owner,
		[]proto.MemberRole{
			admin.toMemberRole(t, proto.AdminRole, tm.hepks),
			member.toMemberRole(t, proto.DefaultRole, tm.hepks),
		}, nil)

	actingAs := team.WrapNamed(proto.FQTeamParsed{
		Team: proto.NewParsedTeamWithFalse(tm.FQTeam(t).Team),
	})
	minder := func(u *TestUser) (libkv.MetaContext, *libkv.Minder) {
		m := libkv.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
		return m, libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
	}
	roles := func(write proto.Role) lcl.KVConfig {
		return lcl.KVConfig{
			ActingAs: actingAs,
			Roles:    proto.RolePairOpt{Read: &proto.DefaultRole, Write: &write},
		}
	}
	plain := lcl.KVConfig{ActingAs: actingAs}

	// A directory every member can write.
	mAdmin, kvAdmin := minder(admin)
	mkdirs := roles(proto.DefaultRole)
	mkdirs.MkdirP = true
	_, err := kvAdmin.Mkdir(mAdmin, mkdirs, "/open")
	require.NoError(t, err)

	mMem, kvMem := minder(member)
	denied := func(err error) {
		t.Helper()
		require.Error(t, err)
		var perm core.KVPermssionError
		require.ErrorAs(t, err, &perm)
	}

	// A member cannot plant names above their own role: a file or a
	// directory.
	_, err = kvMem.PutFileFirst(mMem, roles(proto.OwnerRole), "/open/planted", []byte("x"), true)
	denied(err)
	_, err = kvMem.PutFileFirst(mMem, roles(proto.AdminRole), "/open/planted", []byte("x"), true)
	denied(err)
	_, err = kvMem.Mkdir(mMem, roles(proto.OwnerRole), "/open/emoji")
	denied(err)

	// So an admin can still use the names.
	_, err = kvAdmin.Mkdir(mAdmin, roles(proto.AdminRole), "/open/emoji")
	require.NoError(t, err)

	// What a member may do is unchanged: names at their own role, which an
	// admin can remove; and an admin's names at the admin's role.
	_, err = kvMem.PutFileFirst(mMem, roles(proto.DefaultRole), "/open/mine", []byte("x"), true)
	require.NoError(t, err)
	require.NoError(t, kvAdmin.Unlink(mAdmin, plain, "/open/mine"))
	_, err = kvAdmin.PutFileFirst(mAdmin, roles(proto.AdminRole), "/open/admins", []byte("x"), true)
	require.NoError(t, err)
	denied(kvMem.Unlink(mMem, plain, "/open/admins"))
	// An admin cannot plant a name only owners change either.
	_, err = kvAdmin.PutFileFirst(mAdmin, roles(proto.OwnerRole), "/open/owners", []byte("x"), true)
	denied(err)
	mOwner, kvOwner := minder(owner)
	_, err = kvOwner.PutFileFirst(mOwner, roles(proto.OwnerRole), "/open/owners", []byte("x"), true)
	require.NoError(t, err)
}
