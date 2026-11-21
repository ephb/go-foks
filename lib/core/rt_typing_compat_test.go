// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package core

import (
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/keybase/go-codec/codec"
	"github.com/stretchr/testify/require"
)

// The typing fields were appended to rtPollInbox's argument and result. These
// are the shapes a peer built before that has, to check that old and new
// still read each other: an old server must take a new client's poll, and a
// new client an old server's answer.
type oldRTPollInboxArg struct {
	_struct struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	AppID   *proto.RTAppIDInternal__
	Since   *proto.RTInboxVersionInternal__
	Timeout *proto.DurationMilliInternal__
}

type oldRTInboxPollRes struct {
	_struct      struct{} `codec:",toarray"` //lint:ignore U1000 msgpack internal field
	Bumped       *bool
	InboxVersion *proto.RTInboxVersionInternal__
}

func TestRTPollInboxTypingFieldsAreAdditive(t *testing.T) {
	// New client, old server.
	arg := rem.RTPollInboxArg{
		AppID:       proto.RTAppID_Chat,
		Since:       7,
		Timeout:     25000,
		Typing:      true,
		TypingSince: 99,
	}
	b, err := EncodeToBytes(&arg)
	require.NoError(t, err)
	var oldArg oldRTPollInboxArg
	require.NoError(t, codec.NewDecoderBytes(b, Codec()).Decode(&oldArg))
	require.Equal(t, proto.RTInboxVersion(7), oldArg.Since.Import())
	require.Equal(t, proto.DurationMilli(25000), oldArg.Timeout.Import())

	// Old server, new client: the missing fields read as "no typing".
	bumped := true
	vers := proto.RTInboxVersion(8)
	b = nil
	require.NoError(t, codec.NewEncoderBytes(&b, Codec()).Encode(&oldRTInboxPollRes{
		Bumped:       &bumped,
		InboxVersion: vers.Export(),
	}))
	var res proto.RTInboxPollRes
	require.NoError(t, DecodeFromBytes(&res, b))
	require.True(t, res.Bumped)
	require.Equal(t, vers, res.InboxVersion)
	require.Empty(t, res.Typing)
	require.Zero(t, res.TypingCursor)

	// Old client, new server: the missing fields read as "did not ask".
	b = nil
	app := proto.RTAppID_Chat
	since := proto.RTInboxVersion(7)
	require.NoError(t, codec.NewEncoderBytes(&b, Codec()).Encode(&oldRTPollInboxArg{
		AppID: app.Export(),
		Since: since.Export(),
	}))
	var newArg rem.RTPollInboxArg
	require.NoError(t, DecodeFromBytes(&newArg, b))
	require.Equal(t, since, newArg.Since)
	require.False(t, newArg.Typing)
	require.Zero(t, newArg.TypingSince)
}
