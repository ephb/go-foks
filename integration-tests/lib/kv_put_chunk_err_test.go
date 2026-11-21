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

// A chunk that cannot be sent because its party cannot be loaded is an error
// for the caller. PutFileChunk used to return nil there, so an upload that
// had lost its party (here: a team that does not exist) looked finished.
func TestKVPutFileChunkReportsPartyError(t *testing.T) {
	tew := testEnvBeta(t)
	bluey := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)
	mc := libkv.NewMetaContext(tew.NewClientMetaContext(t, bluey))
	kvm := libkv.NewMinderWithCacheSettings(mc.G().ActiveUser(), libclient.CacheSettings{})

	nm, err := core.RandomDomain()
	require.NoError(t, err)
	cfg := lcl.KVConfig{
		ActingAs: team.WrapNamed(proto.FQTeamParsed{
			Team: proto.NewParsedTeamWithTrue(proto.NameUtf8("no-such-team-" + nm)),
		}),
	}
	var fid proto.FileID
	require.NoError(t, core.RandomFill(fid[:]))
	err = kvm.PutFileChunk(mc, cfg, fid, []byte("more"), proto.Offset(4096), true)
	require.Error(t, err)
}
