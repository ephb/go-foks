// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package team

import (
	"testing"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/stretchr/testify/require"
)

type leaveFixture struct {
	alice, bob, char, dee, team MemberID
	owner, admin, reader, none  core.RoleKey
	roster                      *RosterCore
	keys                        KeyGens
}

func newLeaveFixture(t *testing.T) *leaveFixture {
	f := &leaveFixture{
		alice:  testFQE(t, 1, 1),
		bob:    testFQE(t, 1, 2),
		char:   testFQE(t, 1, 3),
		dee:    testFQE(t, 1, 4),
		owner:  core.OwnerRole,
		admin:  core.AdminRole,
		reader: core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: proto.VizLevel(0)},
		none:   core.RoleKey{Typ: proto.RoleType_NONE},
	}
	f.team = testFQE(t, 1, 5)
	f.team.Fqe.Entity[0] = byte(proto.EntityType_NamedTeam)

	f.roster = NewRosterCore()
	f.roster.Add(f.alice, f.owner, 1, 0, 0)
	f.roster.Add(f.bob, f.owner, 1, 0, 0)
	f.roster.Add(f.char, f.admin, 1, 0, 0)
	f.roster.Add(f.dee, f.reader, 1, 0, 0)
	f.roster.Add(f.team, f.reader, 1, 0, 0)
	f.keys = KeyGens{f.owner: 2, f.admin: 3, f.reader: 4}
	return f
}

func (f *leaveFixture) leave(m MemberID) ChangeSet {
	return ChangeSet{{Member: m, Info: MemberInfo{Role: f.none, Seqno: 7, Time: 1234}}}
}

var leaveOpts = &GameplanOpts{Leave: true}

func TestTeamLeaveMember(t *testing.T) {
	f := newLeaveFixture(t)

	rPost, sched, keysPost, err := f.leave(f.dee).Gameplan(f.dee, f.roster, f.keys, leaveOpts)
	require.NoError(t, err)
	require.False(t, rPost.Has(f.dee))
	require.Equal(t, f.roster.Len()-1, rPost.Len())
	// No new keys: the leaver would know them.
	require.Empty(t, sched.Items)
	require.Equal(t, []MemberID{f.dee}, sched.Removals)
	require.Equal(t, f.keys, keysPost)

	// The same change by an admin, without Leave, rotates as before.
	_, sched, _, err = f.leave(f.dee).Gameplan(f.char, f.roster, f.keys, nil)
	require.NoError(t, err)
	require.Len(t, sched.Items, 1)
	require.True(t, sched.Items[0].NewKeyGen)
}

func TestTeamLeaveRefused(t *testing.T) {
	f := newLeaveFixture(t)

	// A member can't make someone else leave.
	_, _, _, err := f.leave(f.char).Gameplan(f.dee, f.roster, f.keys, leaveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only remove themselves")

	// Not even an owner, with a leave link.
	_, _, _, err = f.leave(f.dee).Gameplan(f.alice, f.roster, f.keys, leaveOpts)
	require.Error(t, err)

	// Only one change.
	two := append(f.leave(f.dee), Change{Member: f.char, Info: MemberInfo{Role: f.none}})
	_, _, _, err = two.Gameplan(f.dee, f.roster, f.keys, leaveOpts)
	require.Error(t, err)

	// A leave removes; it can't change a role.
	up := ChangeSet{{Member: f.dee, Info: MemberInfo{Role: f.owner, Gen: 1}}}
	_, _, _, err = up.Gameplan(f.dee, f.roster, f.keys, leaveOpts)
	require.Error(t, err)

	// Someone not in the team can't leave it.
	eve := testFQE(t, 1, 9)
	_, _, _, err = f.leave(eve).Gameplan(eve, f.roster, f.keys, leaveOpts)
	require.Error(t, err)

	// A team member leaves through its own admins, not this way.
	_, _, _, err = f.leave(f.team).Gameplan(f.team, f.roster, f.keys, leaveOpts)
	require.Error(t, err)

	// Without the Leave option a member's self removal is refused as before.
	_, _, _, err = f.leave(f.dee).Gameplan(f.dee, f.roster, f.keys, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "privileged role")

	// A new team can't start with a leave.
	_, _, _, err = f.leave(f.dee).Gameplan(f.dee, nil, nil, leaveOpts)
	require.Error(t, err)
}

func TestTeamLeaveOwners(t *testing.T) {
	f := newLeaveFixture(t)

	// An owner who is not the last can leave.
	rPost, _, _, err := f.leave(f.bob).Gameplan(f.bob, f.roster, f.keys, leaveOpts)
	require.NoError(t, err)

	// Now alice is the last owner and can't.
	_, _, _, err = f.leave(f.alice).Gameplan(f.alice, rPost, f.keys, leaveOpts)
	require.Error(t, err)
	require.Contains(t, err.Error(), LastOwnerLeaveMsg)
}

func TestTeamRotateKeys(t *testing.T) {
	f := newLeaveFixture(t)

	rot := &GameplanOpts{Rotate: []core.RoleKey{f.reader}}
	rPost, sched, keysPost, err := ChangeSet{}.Gameplan(f.char, f.roster, f.keys, rot)
	require.NoError(t, err)
	require.Equal(t, f.roster.Len(), rPost.Len())
	require.Equal(t, KeySchedule{
		Items: []KeyScheduleItem{{
			Role:      f.reader,
			Gen:       5,
			NewKeyGen: true,
			Members:   sortedMembers(f.alice, f.bob, f.char, f.dee, f.team),
		}},
		Additions: []MemberID{},
		Removals:  []MemberID{},
	}, *sched)
	require.Equal(t, proto.Generation(5), keysPost[f.reader])

	// An admin can't rotate the owner key; an owner can.
	_, _, _, err = ChangeSet{}.Gameplan(f.char, f.roster, f.keys,
		&GameplanOpts{Rotate: []core.RoleKey{f.owner}})
	require.Error(t, err)
	_, sched, _, err = ChangeSet{}.Gameplan(f.alice, f.roster, f.keys,
		&GameplanOpts{Rotate: []core.RoleKey{f.owner}})
	require.NoError(t, err)
	require.Equal(t, sortedMembers(f.alice, f.bob), sched.Items[0].Members)

	// A plain member can't rotate.
	_, _, _, err = ChangeSet{}.Gameplan(f.dee, f.roster, f.keys, rot)
	require.Error(t, err)

	// No member changes in a rotation, no unknown or repeated roles.
	_, _, _, err = f.leave(f.dee).Gameplan(f.char, f.roster, f.keys, rot)
	require.Error(t, err)
	lev9 := core.RoleKey{Typ: proto.RoleType_MEMBER, Lev: proto.VizLevel(9)}
	_, _, _, err = ChangeSet{}.Gameplan(f.char, f.roster, f.keys,
		&GameplanOpts{Rotate: []core.RoleKey{lev9}})
	require.Error(t, err)
	_, _, _, err = ChangeSet{}.Gameplan(f.char, f.roster, f.keys,
		&GameplanOpts{Rotate: []core.RoleKey{f.reader, f.reader}})
	require.Error(t, err)

	// Not both at once.
	_, _, _, err = f.leave(f.dee).Gameplan(f.dee, f.roster, f.keys,
		&GameplanOpts{Leave: true, Rotate: []core.RoleKey{f.reader}})
	require.Error(t, err)
}

func sortedMembers(m ...MemberID) []MemberID {
	ret := append([]MemberID{}, m...)
	for i := range ret {
		for j := i + 1; j < len(ret); j++ {
			if ret[j].Cmp(ret[i]) < 0 {
				ret[i], ret[j] = ret[j], ret[i]
			}
		}
	}
	return ret
}

func TestTeamRotationDue(t *testing.T) {
	f := newLeaveFixture(t)
	r := NewRoster(f.roster, f.keys, nil)
	require.Empty(t, r.RotationDue)

	leaver := MemberInfo{Role: f.admin}
	leave := f.leave(f.char)[0]
	due := nextRotationDue(r.RotationDue, f.keys, &leaver, &leave, &KeySchedule{})
	// An admin could read the admin and member keys, not the owner key.
	require.Equal(t, RotationDue{
		f.admin:  {Seqno: 7, Time: 1234},
		f.reader: {Seqno: 7, Time: 1234},
	}, due)

	// A second leave keeps the first one's time.
	later := Change{Member: f.dee, Info: MemberInfo{Role: f.none, Seqno: 9, Time: 5678}}
	due = nextRotationDue(due, f.keys, &MemberInfo{Role: f.reader}, &later, &KeySchedule{})
	require.Equal(t, proto.Seqno(7), due[f.reader].Seqno)

	// A rotation of the member key clears it; the admin key stays due.
	due = nextRotationDue(due, f.keys, nil, nil, &KeySchedule{
		Items: []KeyScheduleItem{{Role: f.reader, NewKeyGen: true}},
	})
	require.Equal(t, RotationDue{f.admin: {Seqno: 7, Time: 1234}}, due)

	// Boxing an existing key for someone new is not a rotation.
	due = nextRotationDue(due, f.keys, nil, nil, &KeySchedule{
		Items: []KeyScheduleItem{{Role: f.admin, NewKeyGen: false}},
	})
	require.Len(t, due, 1)
}
