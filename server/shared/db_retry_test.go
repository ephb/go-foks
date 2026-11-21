// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"errors"
	"fmt"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
)

// RetryTx2 runs a transaction again when Postgres made it the victim of a
// deadlock or a serialization failure, and for nothing else its function
// returns.
func TestIsTxRestartError(t *testing.T) {
	require.True(t, IsTxRestartError(&pgconn.PgError{Code: "40P01"}))
	require.True(t, IsTxRestartError(&pgconn.PgError{Code: "40001"}))
	require.True(t, IsTxRestartError(fmt.Errorf("send: %w", &pgconn.PgError{Code: "40P01"})))

	require.False(t, IsTxRestartError(nil))
	require.False(t, IsTxRestartError(&pgconn.PgError{Code: "23505"}))
	require.False(t, IsTxRestartError(pgx.ErrNoRows))
	require.False(t, IsTxRestartError(core.RowNotFoundError{}))
	require.False(t, IsTxRestartError(errors.New("deadlock detected")))
}
