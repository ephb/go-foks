// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

// A Minder with SetNoLinks follows no symlink, wherever it is in the path,
// and still works on the link itself.
func TestKVNoLinks(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)
	tm := tew.makeTeamForOwner(t, owner)

	m := libkv.NewMetaContext(tew.NewClientMetaContextWithEracer(t, owner))
	cfg := lcl.KVConfig{
		ActingAs: team.WrapNamed(proto.FQTeamParsed{
			Team: proto.NewParsedTeamWithFalse(tm.FQTeam(t).Team),
		}),
		Roles:  proto.RolePairOpt{Read: &proto.DefaultRole, Write: &proto.DefaultRole},
		MkdirP: true,
	}
	plain := cfg
	plain.MkdirP = false
	nofollow := plain
	nofollow.NoFollow = true

	// The tree, made by a Minder that follows links (the stock client):
	// /private/secret, /shared/x -> /private, /top -> /private.
	stock := libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
	_, err := stock.PutFileFirst(m, cfg, "/private/secret", []byte("s3cret"), true)
	require.NoError(t, err)
	_, err = stock.Mkdir(m, cfg, "/shared")
	require.NoError(t, err)
	_, err = stock.Symlink(m, plain, "/shared/x", "/private")
	require.NoError(t, err)
	_, err = stock.Symlink(m, plain, "/top", "/private")
	require.NoError(t, err)
	_, err = stock.GetFile(m, plain, "/shared/x/secret")
	require.NoError(t, err, "the stock client follows the link")

	kvm := libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})
	kvm.SetNoLinks(true)
	refused := func(err error, link string) {
		t.Helper()
		var lerr libkv.LinkInPathError
		require.ErrorAs(t, err, &lerr)
		require.Equal(t, proto.KVPath(link), lerr.Path)
	}

	// Through a link in the middle or at the end: read, stat, list, write,
	// mkdir (also mkdir -p, and mkdir of the link's own name), move into.
	_, err = kvm.GetFile(m, plain, "/shared/x/secret")
	refused(err, "/shared/x")
	_, err = kvm.Stat(m, plain, "/shared/x/secret")
	refused(err, "/shared/x")
	_, err = kvm.Stat(m, nofollow, "/shared/x/secret")
	refused(err, "/shared/x")
	_, err = kvm.Stat(m, plain, "/top")
	refused(err, "/top")
	_, err = kvm.List(m, plain, "/shared/x", nil, rem.KVListOpts{Num: 10})
	refused(err, "/shared/x")
	_, err = kvm.PutFileFirst(m, cfg, "/shared/x/new", []byte("x"), true)
	refused(err, "/shared/x")
	// A put never follows a link at the end of its path, with or without
	// this: the name is taken.
	_, err = kvm.PutFileFirst(m, cfg, "/top", []byte("x"), true)
	require.Error(t, err)
	_, err = kvm.Mkdir(m, cfg, "/shared/x/dir/deeper")
	refused(err, "/shared/x")
	_, err = kvm.Mkdir(m, plain, "/shared/x")
	refused(err, "/shared/x")
	_, err = kvm.Mkdir(m, cfg, "/top")
	refused(err, "/top")
	_, err = kvm.PutFileFirst(m, cfg, "/shared/file", []byte("f"), true)
	require.NoError(t, err)
	err = kvm.Mv(m, plain, "/shared/file", "/shared/x/file")
	refused(err, "/shared/x")
	err = kvm.Mv(m, plain, "/shared/file", "/top")
	refused(err, "/top")
	err = kvm.Unlink(m, plain, "/shared/x/secret")
	refused(err, "/shared/x")

	// Nothing was written behind the link.
	lst, err := stock.List(m, plain, "/private", nil, rem.KVListOpts{Num: 10})
	require.NoError(t, err)
	require.Len(t, lst.Ents, 1)

	// The link itself: stat without following, read, move, remove.
	st, err := kvm.Stat(m, nofollow, "/shared/x")
	require.NoError(t, err)
	typ, err := st.V.GetT()
	require.NoError(t, err)
	require.Equal(t, proto.KVNodeType_Symlink, typ)
	target, err := kvm.Readlink(m, plain, "/shared/x")
	require.NoError(t, err)
	require.Equal(t, proto.KVPath("/private"), *target)
	require.NoError(t, kvm.Mv(m, plain, "/shared/x", "/shared/y"))
	require.NoError(t, kvm.Unlink(m, plain, "/shared/y"))
	require.NoError(t, kvm.Unlink(m, plain, "/top"))

	// And paths without links work as ever.
	_, err = kvm.GetFile(m, plain, "/private/secret")
	require.NoError(t, err)
	_, err = kvm.Mkdir(m, cfg, "/shared/a/b")
	require.NoError(t, err)
}
