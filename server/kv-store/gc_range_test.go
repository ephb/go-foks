// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package kvStore

import (
	"bytes"
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// The range holds every ID that starts with the short ID and nothing else.
func TestShortPartyIDRange(t *testing.T) {
	var spid proto.ShortPartyID
	spid[0] = 1
	spid[16] = 7
	lo, hi := shortPartyIDRange(spid)
	require.Equal(t, spid[:], lo)
	require.Len(t, hi, 17)
	require.Equal(t, byte(8), hi[16])
	full := append(append([]byte{}, spid[:]...), 0xff, 0xff)
	require.True(t, bytes.Compare(full, lo) >= 0 && bytes.Compare(full, hi) < 0)

	// A carry shortens the bound.
	spid[16] = 0xff
	spid[15] = 0xff
	spid[14] = 3
	lo, hi = shortPartyIDRange(spid)
	require.Equal(t, spid[:], lo)
	require.Equal(t, append(append([]byte{}, spid[:14]...), 4), hi)
	full = append(append([]byte{}, spid[:]...), 0xff)
	require.True(t, bytes.Compare(full, lo) >= 0 && bytes.Compare(full, hi) < 0)

	for i := range spid {
		spid[i] = 0xff
	}
	_, hi = shortPartyIDRange(spid)
	require.Nil(t, hi)
}
