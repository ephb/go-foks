// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A channel marked too large admits nothing until the mark runs out, so the
// caller does not read its roster again in that time.
func TestRTTypingHubTooLarge(t *testing.T) {
	h := NewRTTypingHub()
	alice, bob := typingTestUID(1), typingTestUID(2)
	t0 := time.Unix(1_000_000, 0)
	big, small := typingTestChan(7), typingTestChan(8)

	require.True(t, h.Admit(big, alice, true, t0))
	h.MarkTooLarge(big, t0)

	require.False(t, h.Admit(big, bob, true, t0.Add(time.Second)))
	require.False(t, h.Admit(big, alice, false, t0.Add(time.Second)))
	require.False(t, h.Admit(big, alice, true, t0.Add(RTTypingTooLargeTTL-time.Millisecond)))
	require.True(t, h.Admit(small, bob, true, t0.Add(time.Second)), "another channel is not held")

	// When the mark ran out the caller gets to look at the roster again.
	require.True(t, h.Admit(big, bob, true, t0.Add(RTTypingTooLargeTTL)))

	// A clock set back does not keep the mark for longer than its time.
	h.MarkTooLarge(big, t0.Add(time.Hour))
	require.True(t, h.Admit(big, alice, true, t0.Add(2*time.Minute)))

	// The sweep drops marks that ran out.
	h.MarkTooLarge(small, t0.Add(3*time.Minute))
	h.Admit(typingTestChan(9), alice, false, t0.Add(24*time.Hour))
	h.Lock()
	n := len(h.tooLarge)
	h.Unlock()
	require.Zero(t, n)
}
