// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	kvStore "github.com/foks-proj/go-foks/server/kv-store"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// TestKVStorePatchP1 upgrades a KV shard the way `foks-tool patch-db --db
// kv-store` does. The test database is made from the base schema file, which
// already holds the patch, so the test first puts every shard back to how a
// server from before the patch looks: no GC indexes and no schema_patches
// table (p1 is the first patch for the KV shards).
func TestKVStorePatchP1(t *testing.T) {
	tew := testEnvBeta(t)
	m := tew.MetaContext()

	gcIndexes := func() int {
		var tot int
		err := m.ForAllShards(func(db *pgxpool.Conn) error {
			var n int
			err := db.QueryRow(
				m.Ctx(),
				`SELECT COUNT(*) FROM pg_indexes
				 WHERE (tablename = 'large_file'
				   AND indexname IN ('large_file_gc_unlinked_idx', 'large_file_gc_uploading_idx'))
				    OR (tablename = 'dirent' AND indexname = 'dirent_value_active_idx')`,
			).Scan(&n)
			tot += n
			return err
		})
		require.NoError(t, err)
		return tot
	}
	var nShards int
	err := m.ForAllShards(func(db *pgxpool.Conn) error {
		nShards++
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, nShards, 0)
	require.Equal(t, 3*nShards, gcIndexes(), "a fresh install has the indexes")

	run := func() error {
		eng := &shared.PatchDBEng{
			Which: shared.DbTypeKVStore,
			ConfirmHook: func(m shared.MetaContext, ps *shared.PatchSummary) error {
				require.Len(t, ps.Desc(), nShards)
				return nil
			},
		}
		return eng.Run(m)
	}

	// A fresh install has nothing to patch.
	err = run()
	require.Equal(t, core.NotFoundError("no patches to apply"), err)

	err = m.ForAllShards(func(db *pgxpool.Conn) error {
		for _, q := range []string{
			`DROP INDEX large_file_gc_unlinked_idx`,
			`DROP INDEX large_file_gc_uploading_idx`,
			`DROP INDEX dirent_value_active_idx`,
			`DROP TABLE schema_patches`,
		} {
			_, err := db.Exec(m.Ctx(), q)
			if err != nil {
				return err
			}
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 0, gcIndexes())

	// The garbage collector refuses a shard without the patch, and says
	// what to run.
	gcOpts := kvStore.GCOptsFromConfig(shared.DefaultKVGCConfig())
	st, err := kvStore.CollectGarbage(m, gcOpts)
	require.Error(t, err)
	require.IsType(t, core.ConfigError(""), err)
	require.Contains(t, err.Error(), "patch-db --db kv-store")
	require.Equal(t, kvStore.GCStats{}, st)

	err = run()
	require.NoError(t, err)
	_, err = kvStore.CollectGarbage(m, gcOpts)
	require.NoError(t, err)
	require.Equal(t, 3*nShards, gcIndexes(), "the patch made the indexes")

	err = m.ForAllShards(func(db *pgxpool.Conn) error {
		var n int
		err := db.QueryRow(m.Ctx(), `SELECT COUNT(*) FROM schema_patches WHERE id = 1`).Scan(&n)
		if err != nil {
			return err
		}
		if n != 1 {
			return core.NotFoundError("schema_patches row")
		}
		return nil
	})
	require.NoError(t, err)

	// And a second run finds nothing left.
	err = run()
	require.Equal(t, core.NotFoundError("no patches to apply"), err)
}
