// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/client/libkv"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// TestKVGetFileChunkUnalignedOffset reads a large file by id at offsets that
// aren't chunk boundaries. The server hands back the chunk that holds the
// offset; the client used to derive the nonce from the requested offset (so
// decryption failed) and to slice the chunk the wrong way round.
func TestKVGetFileChunkUnalignedOffset(t *testing.T) {
	tew := testEnvBeta(t)
	u := tew.NewTestUser(t)
	tew.DirectMerklePokeInTest(t)

	m := libkv.NewMetaContext(tew.NewClientMetaContextWithEracer(t, u))
	kvm := libkv.NewMinderWithCacheSettings(m.G().ActiveUser(), libclient.CacheSettings{})

	root, err := core.RandomDomain()
	require.NoError(t, err)
	path := proto.KVPath("/" + root + "/f.bin")
	cfg := lcl.KVConfig{MkdirP: true}
	buf := writeRandomFileWithConfig(t, kvm, m, path, 3*testChunkSize+77, cfg, 0)

	gfr, err := kvm.GetFile(m, cfg, path)
	require.NoError(t, err)
	require.NotNil(t, gfr.Id)

	for _, off := range []int{0, 1, testChunkSize - 1, testChunkSize, testChunkSize + 123, 3*testChunkSize + 5} {
		c, err := kvm.GetFileChunk(m, cfg, *gfr.Id, proto.Offset(off))
		require.NoError(t, err, "offset %d", off)
		end := min((off/testChunkSize+1)*testChunkSize, len(buf))
		require.Equal(t, buf[off:end], []byte(c.Chunk), "offset %d", off)
	}
}
