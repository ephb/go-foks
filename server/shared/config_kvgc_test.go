// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKVGCConfig(t *testing.T) {
	// No kv_store block, or no gc block: the GC is off, with the defaults.
	var none *KVStoreServerConfigJSON
	require.Equal(t, DefaultKVGCConfig(), none.GC())
	require.False(t, none.GC().Enabled)
	require.Equal(t, DefaultKVGCConfig(), (&KVStoreServerConfigJSON{}).GC())

	var k KVStoreServerConfigJSON
	err := json.Unmarshal([]byte(`{"blob_store_path":"sql","gc":{"enabled":true,"upload_grace_sec":7200}}`), &k)
	require.NoError(t, err)
	g := k.GC()
	require.True(t, g.Enabled)
	require.Equal(t, 2*time.Hour, g.UploadGrace)
	require.Equal(t, DefaultKVGCConfig().UnlinkedGrace, g.UnlinkedGrace)
	require.Equal(t, DefaultKVGCConfig().Interval, g.Interval)
	require.Equal(t, DefaultKVGCConfig().BatchSize, g.BatchSize)
}
