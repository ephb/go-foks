// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package lib

import (
	"errors"
	"testing"

	"github.com/foks-proj/go-foks/client/libclient"
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	"github.com/foks-proj/go-foks/proto/lcl"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

// leaveFixture: alice owns a team; bob joined it as a member and carol as
// an admin, the way the apps add people (invite, accept, admit).
type leaveFixture struct {
	tew   *TestEnvWrapper
	tm    *teamObj
	fqtp  proto.FQTeamParsed
	fqt   proto.FQTeam
	alice leaveParty
	bob   leaveParty
	carol leaveParty
}

type leaveParty struct {
	u  *TestUser
	m  libclient.MetaContext
	tm *libclient.TeamMinder
}

func newLeaveFixture(t *testing.T) *leaveFixture {
	tew := testEnvBeta(t)
	f := &leaveFixture{tew: tew}
	mk := func() leaveParty {
		u := tew.NewTestUser(t)
		return leaveParty{u: u}
	}
	f.alice, f.bob, f.carol = mk(), mk(), mk()
	tew.DirectDoubleMerklePokeInTest(t)

	f.tm = tew.makeTeamForOwner(t, f.alice.u)
	tew.DirectDoubleMerklePokeInTest(t)
	f.tm.setIndexRange(t, tew.MetaContext(), f.alice.u, index0)
	tew.DirectDoubleMerklePokeInTest(t)
	f.fqtp = *f.tm.ToFQTeamParsed(t)
	f.fqt = f.tm.FQTeam(t)

	for _, p := range []*leaveParty{&f.alice, &f.bob, &f.carol} {
		p.m, p.tm = tew.teamMinderFor(t, p.u)
	}

	inv, err := f.alice.tm.CreateInvite(f.alice.m, f.fqtp)
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)
	for _, p := range []leaveParty{f.bob, f.carol} {
		_, err = p.tm.AcceptInvite(p.m, lcl.TeamAcceptInviteArg{I: *inv})
		require.NoError(t, err)
		tew.DirectDoubleMerklePokeInTest(t)
	}
	inbox, err := f.alice.tm.TeamInbox(f.alice.m, f.fqtp)
	require.NoError(t, err)
	require.Len(t, inbox.Rows, 2)
	var toks []lcl.TokRole
	for _, row := range inbox.Rows {
		role := proto.DefaultRole
		if row.Nfqp.Fqp.Party.EntityID().Eq(f.carol.u.uid.EntityID()) {
			role = proto.AdminRole
		}
		toks = append(toks, lcl.TokRole{Tok: row.Tok, Role: role})
	}
	err = f.alice.tm.TeamAdmit(f.alice.m, lcl.TeamAdmitArg{Team: f.fqtp, Members: toks})
	require.NoError(t, err)
	tew.DirectDoubleMerklePokeInTest(t)

	for _, p := range []leaveParty{f.bob, f.carol} {
		require.True(t, f.lists(t, p), "member should list the team")
	}
	return f
}

// lists: does p's ListMemberships have the team?
func (f *leaveFixture) lists(t *testing.T, p leaveParty) bool {
	res, err := p.tm.ListMemberships(p.m, &libclient.LoadTeamOpts{Refresh: true})
	require.NoError(t, err)
	for _, tm := range res.Teams {
		if tm.Team.Fqp.Party.EntityID().Eq(f.fqt.Team.EntityID()) {
			return true
		}
	}
	return false
}

func (f *leaveFixture) roster(t *testing.T, p leaveParty) []proto.UID {
	r, err := p.tm.ListTeamRoster(p.m, f.fqtp)
	require.NoError(t, err)
	var ret []proto.UID
	for _, mem := range r.Members {
		uid, err := mem.Mem.Fqp.Party.UID()
		if err == nil {
			ret = append(ret, uid)
		}
	}
	return ret
}

func (f *leaveFixture) keyGen(t *testing.T, p leaveParty, role proto.Role) proto.Generation {
	tr, err := p.tm.LoadTeamWithFQTeam(p.m, f.fqt, libclient.LoadTeamOpts{Refresh: true})
	require.NoError(t, err)
	rk, err := core.ImportRole(role)
	require.NoError(t, err)
	ks := tr.Tw().KeyRing().KeysForRole(*rk)
	require.NotNil(t, ks)
	return ks.LastGen()
}

func (f *leaveFixture) due(t *testing.T, p leaveParty) team.RotationDue {
	d, err := p.tm.RotationDue(p.m, f.fqt)
	require.NoError(t, err)
	return d
}

func (f *leaveFixture) leave(t *testing.T, p leaveParty) error {
	err := p.tm.TeamLeave(p.m, f.fqtp, libclient.TeamLeaveOpts{})
	f.tew.DirectDoubleMerklePokeInTest(t)
	return err
}

func leaveRefusal(err error) libclient.TeamLeaveRefusal {
	var le libclient.TeamLeaveError
	if errors.As(err, &le) {
		return le.Which
	}
	return 0
}

// A member leaves at once; the keys stay until an admin's client rotates
// them, and then the leaver can't get the new ones.
func TestTeamLeaveAndRotate(t *testing.T) {
	f := newLeaveFixture(t)
	genBefore := f.keyGen(t, f.alice, proto.DefaultRole)
	require.Empty(t, f.due(t, f.alice))

	announced := false
	err := f.bob.tm.TeamLeave(f.bob.m, f.fqtp, libclient.TeamLeaveOpts{
		BeforePost: func(libclient.MetaContext) { announced = true },
	})
	require.NoError(t, err)
	require.True(t, announced)
	f.tew.DirectDoubleMerklePokeInTest(t)

	// Out at once: bob's client lists the team no more, and the server
	// refuses to load it for him.
	require.False(t, f.lists(t, f.bob))
	_, err = f.bob.tm.LoadTeamWithFQTeam(f.bob.m, f.fqt, libclient.LoadTeamOpts{Refresh: true})
	require.Error(t, err)

	// The others see him gone, and the member keys due.
	require.NotContains(t, f.roster(t, f.alice), f.bob.u.uid)
	due := f.due(t, f.alice)
	require.Contains(t, due, core.MemberRole)
	require.NotContains(t, due, core.AdminRole)
	require.NotContains(t, due, core.OwnerRole)
	require.Equal(t, genBefore, f.keyGen(t, f.alice, proto.DefaultRole))

	// Leaving twice: the team is not his any more.
	err = f.leave(t, f.bob)
	require.Error(t, err)

	// carol (admin) rotates; alice, racing behind her, has nothing left.
	res, err := f.carol.tm.RotateKeysIfDue(f.carol.m, f.fqt)
	require.NoError(t, err)
	require.Contains(t, res.Rotated, core.MemberRole)
	require.Empty(t, res.StillDue)
	f.tew.DirectDoubleMerklePokeInTest(t)
	res, err = f.alice.tm.RotateKeysIfDue(f.alice.m, f.fqt)
	require.NoError(t, err)
	require.Empty(t, res.Rotated)

	require.Empty(t, f.due(t, f.alice))
	require.Empty(t, f.due(t, f.carol))
	require.Equal(t, genBefore+1, f.keyGen(t, f.alice, proto.DefaultRole))
	require.Equal(t, genBefore+1, f.keyGen(t, f.carol, proto.DefaultRole))

	// bob's device can't load the team (and so its new keys) directly
	// either, with the keys it has.
	puks, err := f.bob.m.G().ActiveUser().RefreshPUKs(f.bob.m)
	require.NoError(t, err)
	_, err = libclient.LoadTeam(f.bob.m, libclient.LoadTeamArg{
		As:      f.bob.m.G().ActiveUser().FQParty(),
		Team:    f.fqt,
		Keys:    puks,
		SrcRole: proto.OwnerRole,
	})
	require.Error(t, err)
}

// The last owner can't leave; an owner who is not the last can, and then
// only an owner can rotate the owner key.
func TestTeamLeaveOwners(t *testing.T) {
	f := newLeaveFixture(t)

	err := f.leave(t, f.alice)
	require.Error(t, err)
	require.Equal(t, libclient.TeamLeaveLastOwner, leaveRefusal(err))
	require.True(t, f.lists(t, f.alice))

	err = f.alice.tm.TeamChangeRoles(f.alice.m, lcl.TeamChangeRolesArg{
		Team: f.fqtp,
		Changes: []lcl.RoleChange{{
			Member:  lcl.FQPartyParsedAndRole{Fqp: proto.FQPartyParsed{Party: proto.NewParsedPartyWithTrue(proto.PartyName{Name: f.carol.u.name})}},
			NewRole: proto.OwnerRole,
		}},
	})
	require.NoError(t, err)
	f.tew.DirectDoubleMerklePokeInTest(t)

	ownerGen := f.keyGen(t, f.carol, proto.OwnerRole)
	err = f.leave(t, f.alice)
	require.NoError(t, err)
	require.False(t, f.lists(t, f.alice))

	due := f.due(t, f.carol)
	require.Contains(t, due, core.OwnerRole)
	require.Contains(t, due, core.AdminRole)

	res, err := f.carol.tm.RotateKeysIfDue(f.carol.m, f.fqt)
	require.NoError(t, err)
	require.Contains(t, res.Rotated, core.OwnerRole)
	f.tew.DirectDoubleMerklePokeInTest(t)
	require.Empty(t, f.due(t, f.carol))
	require.Equal(t, ownerGen+1, f.keyGen(t, f.carol, proto.OwnerRole))

	// carol is now the last owner.
	err = f.leave(t, f.carol)
	require.Equal(t, libclient.TeamLeaveLastOwner, leaveRefusal(err))
}

// An admin who leaves can't rotate the admin key; an owner's client does.
// The admin's team tokens end with the leave.
func TestTeamLeaveAdmin(t *testing.T) {
	f := newLeaveFixture(t)

	err := f.leave(t, f.carol)
	require.NoError(t, err)

	due := f.due(t, f.alice)
	require.Contains(t, due, core.AdminRole)
	require.NotContains(t, due, core.OwnerRole)

	// A plain member's client rotates nothing.
	res, err := f.bob.tm.RotateKeysIfDue(f.bob.m, f.fqt)
	require.NoError(t, err)
	require.Empty(t, res.Rotated)

	res, err = f.alice.tm.RotateKeysIfDue(f.alice.m, f.fqt)
	require.NoError(t, err)
	require.Contains(t, res.Rotated, core.AdminRole)
	require.Contains(t, res.Rotated, core.MemberRole)
	f.tew.DirectDoubleMerklePokeInTest(t)
	require.Empty(t, f.due(t, f.bob))
}
