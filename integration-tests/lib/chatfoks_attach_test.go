//go:build chatfoks

// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

// chat-foks attachments (PLAN-chat 70): chatd's KV store (the shim adapter
// over libkv.Minder, backend/shim/adapter_attach.go) and its sealed
// transfer (backend/shim/attach) against a real FOKS server, as several
// users: a named team with the admin folder step, a DM (ad-hoc team), and
// the refusals (not enabled, non-admin enable, role too low, not in the
// team). Two chatds over the wire: integration-tests/cli
// chatd_attach_test.go.
//
//	go test -tags chatfoks ./integration-tests/lib -run TestChatFoksAttachStore -v

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"

	"github.com/ephb/chat-foks/backend/shim"
	"github.com/ephb/chat-foks/backend/shim/attach"
)

// attShim is the chatd adapter for user u, in this process.
func attShim(t *testing.T, tew *TestEnvWrapper, u *TestUser) shim.Shim {
	m := tew.NewClientMetaContextWithEracer(t, u)
	s, err := shim.NewAdapter(m.G())
	require.NoError(t, err)
	return s
}

func attRandom(t *testing.T, n int) []byte {
	b := make([]byte, n)
	require.NoError(t, core.RandomFill(b))
	return b
}

// attUpload seals and uploads data the way send_attachment does.
func attUpload(t *testing.T, st shim.Shim, cfg lcl.ConfigTeam, roles proto.RolePair, chid string, data []byte) *attach.Sealed {
	ctx := context.Background()
	id, err := attach.NewID()
	require.NoError(t, err)
	key, err := attach.NewKey()
	require.NoError(t, err)
	dir := attach.Dir(chid, id)
	require.NoError(t, st.AttEnsureDir(ctx, cfg, dir, roles))
	fid, sha, err := attach.Upload(ctx, st, cfg, roles, dir+"/file", &key, id,
		bytes.NewReader(data), int64(len(data)), nil)
	require.NoError(t, err)
	return &attach.Sealed{ID: id, Key: key, File: fid, Size: int64(len(data)), SHA256: sha}
}

func attRead(t *testing.T, st shim.Shim, cfg lcl.ConfigTeam, s *attach.Sealed, want []byte) {
	ctx := context.Background()
	var got bytes.Buffer
	require.NoError(t, attach.Download(ctx, st, cfg, s, &got, nil))
	require.True(t, bytes.Equal(want, got.Bytes()), "downloaded file differs")
	// Seeks: inside the first chunk, across a chunk edge, the tail.
	for _, r := range [][2]int64{{17, 1000}, {attach.PlainChunk - 10, 50}, {int64(len(want)) - 5, 100}} {
		b, err := attach.ReadRange(ctx, st, cfg, s, r[0], r[1])
		require.NoError(t, err)
		end := min(r[0]+r[1], int64(len(want)))
		require.True(t, bytes.Equal(want[r[0]:end], b), "range %v", r)
	}
}

func requireCode(t *testing.T, err error, code shim.ErrCode, what string) {
	t.Helper()
	require.Error(t, err, what)
	got, ok := shim.Classify(err)
	require.True(t, ok)
	require.Equal(t, code, got, "%s: %v", what, err)
	t.Logf("%s: %v", what, err)
}

func TestChatFoksAttachStore(t *testing.T) {
	t.Run("named", testChatFoksAttachNamed)
	t.Run("adhoc", testChatFoksAttachAdHoc)
}

func testChatFoksAttachNamed(t *testing.T) {
	tew := testEnvBeta(t)
	owner := tew.NewTestUser(t)
	member := tew.NewTestUser(t)
	member2 := tew.NewTestUser(t)
	lowly := tew.NewTestUser(t)
	outsider := tew.NewTestUser(t)
	tew.DirectDoubleMerklePokeInTest(t)

	// A team made outside chatd: no /chat-foks folder yet.
	tm := tew.makeTeamForOwner(t, owner)
	lowRole := proto.NewRoleWithMember(proto.VizLevel(-10))
	tm.makeChanges(t, tew.MetaContext(), owner, []proto.MemberRole{
		member.toMemberRole(t, proto.DefaultRole, tm.hepks),
		member2.toMemberRole(t, proto.DefaultRole, tm.hepks),
		lowly.toMemberRole(t, lowRole, tm.hepks),
	}, nil)
	tew.DirectDoubleMerklePokeInTest(t)

	cfg := team.WrapNamed(proto.FQTeamParsed{Team: proto.NewParsedTeamWithFalse(tm.FQTeam(t).Team)})
	roles := proto.RolePair{Read: proto.DefaultRole, Write: proto.DefaultRole}
	chid := "00112233445566778899aabbccddeeff"
	ctx := context.Background()

	sMember := attShim(t, tew, member)
	id, _ := attach.NewID()
	err := sMember.AttEnsureDir(ctx, cfg, attach.Dir(chid, id), roles)
	requireCode(t, err, shim.ErrRejected, "member uploads before the folder exists")
	require.True(t, errors.As(err, new(shim.AttachmentsNotEnabledError)))
	requireCode(t, sMember.EnableAttachments(ctx, cfg), shim.ErrAuth, "member runs enable_attachments")

	sOwner := attShim(t, tew, owner)
	require.NoError(t, sOwner.EnableAttachments(ctx, cfg))
	require.NoError(t, sOwner.EnableAttachments(ctx, cfg), "enable_attachments twice")

	// Now the member sends 5 MiB and a tiny file (padded past KV's small
	// file limit), and another member reads both.
	data := attRandom(t, 5<<20+12345)
	big := attUpload(t, sMember, cfg, roles, chid, data)
	tiny := []byte("hi")
	small := attUpload(t, sMember, cfg, roles, chid, tiny)
	sMember2 := attShim(t, tew, member2)
	attRead(t, sMember2, cfg, big, data)
	var got bytes.Buffer
	require.NoError(t, attach.Download(ctx, sMember2, cfg, small, &got, nil))
	require.Equal(t, tiny, got.Bytes())

	// A member below the channel's read role, and a non-member.
	sLowly := attShim(t, tew, lowly)
	_, err = attach.ReadRange(ctx, sLowly, cfg, big, 0, 10)
	requireCode(t, err, shim.ErrAuth, "member below the read role reads")
	sOut := attShim(t, tew, outsider)
	_, err = attach.ReadRange(ctx, sOut, cfg, big, 0, 10)
	requireCode(t, err, shim.ErrAuth, "non-member reads")
	err = sOut.AttEnsureDir(ctx, cfg, attach.Dir(chid, id), roles)
	requireCode(t, err, shim.ErrAuth, "non-member uploads")
}

func testChatFoksAttachAdHoc(t *testing.T) {
	f := createAdHocTeamForTest(t, 1)
	tew := f.tew
	bob := f.others[0]
	cfg := lcl.NewConfigTeamWithAdhoc(proto.FQAdHocTeamParsed{
		Team: proto.NewAdHocTeamParsedWithId(f.tid.EntityID()),
	})
	roles := proto.RolePair{Read: proto.DefaultRole, Write: proto.DefaultRole}
	chid := "ffeeddccbbaa99887766554433221100"

	// Everyone in a DM is an owner: no admin step.
	data := attRandom(t, 3<<20+7)
	s := attUpload(t, attShim(t, tew, f.alice), cfg, roles, chid, data)
	attRead(t, attShim(t, tew, bob), cfg, s, data)

	carol := tew.NewTestUserAtVHost(t, f.vhost)
	tew.DirectDoubleMerklePokeInTest(t)
	_, err := attach.ReadRange(context.Background(), attShim(t, tew, carol), cfg, s, 0, 10)
	requireCode(t, err, shim.ErrAuth, "carol, not in the DM, reads")
}
