// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package team

import (
	"maps"

	"github.com/foks-proj/go-foks/lib/core"
	proto "github.com/foks-proj/go-foks/proto/lib"
)

// Leaving a team, and rotating its keys afterwards.
//
// Every other change to a team's roster is made by an admin or owner, and
// whoever makes a change that removes someone also makes the new keys that
// person must not read (planChangesLocked, keyFlood). A member who leaves
// cannot do that: the leaver would know the "new" keys. So leaving is two
// links:
//
//  1. A TeamLeave link, signed by the member who leaves. It has exactly one
//     change -- the signer to role NONE -- and no new keys. Any member may
//     make it about themselves, and nobody may make it about anyone else.
//     The last owner may not leave (the team would have no owner).
//  2. A TeamRotateKeys link, signed by an admin or owner who stays. It has
//     no member changes and makes a new generation of the keys of the given
//     roles, boxed for every member who can read them, the same way a
//     removal by an admin does. A signer may only rotate roles at or below
//     its own.
//
// Between the two, the roles the leaver could read are "due" (Roster.
// RotationDue). Nothing in the chain rules forces the second link: the
// clients of the team's admins make it as soon as they see the first (the
// same lazy pattern as CLKR). The server stops serving the leaver at once,
// because it records the removal like any other.

// LastOwnerLeaveMsg: the last owner of a team asked to leave it.
const LastOwnerLeaveMsg = "the last owner can't leave the team"

// RotationDueInfo is the first leave since a role's current key
// generation: the link (seqno) and its time.
type RotationDueInfo struct {
	Seqno proto.Seqno
	Time  proto.Time
}

// RotationDue maps each role whose current key a departed member could read
// to the leave that made it so.
type RotationDue map[core.RoleKey]RotationDueInfo

func (r RotationDue) Clone() RotationDue {
	ret := make(RotationDue, len(r))
	maps.Copy(ret, r)
	return ret
}

// checkLeaveLocked checks a TeamLeave change set: the doer, a user who is in
// the roster at any role, removes itself and nothing else.
func (r *RosterCore) checkLeaveLocked(doer MemberID, changes ChangeSet) error {
	if len(changes) != 1 {
		return core.TeamRosterError("a leave must have exactly one change")
	}
	chng := changes[0]
	if !chng.Member.Eq(doer) {
		return core.TeamRosterError("a member can only remove themselves")
	}
	if chng.Info.Role.Typ != proto.RoleType_NONE {
		return core.TeamRosterError("a leave must remove the member")
	}
	if !chng.Info.Gen.IsVoid() {
		return core.TeamRosterError("must supply 0-generation for removal")
	}
	if !doer.Fqe.Unfix().Entity.Type().IsUser() {
		return core.TeamRosterError("only a user can leave a team this way")
	}
	if _, ok := r.members[doer]; !ok {
		return core.TeamRosterError("doer not in roster")
	}
	return nil
}

// checkRotateLocked checks a TeamRotateKeys link: no member changes, and
// each role named once, keyed already, and not above the doer's role. The
// caller has checked (checkChangesLocked) that the doer is an admin or
// owner in the roster.
func (r *RosterCore) checkRotateLocked(
	doer MemberID,
	changes ChangeSet,
	keysPre KeyGens,
	roles []core.RoleKey,
) error {
	if len(changes) != 0 {
		return core.TeamRosterError("a key rotation can't change members")
	}
	if len(roles) == 0 {
		return core.TeamRosterError("a key rotation needs at least one role")
	}
	doerInfo, ok := r.members[doer]
	if !ok {
		return core.TeamRosterError("doer not in roster")
	}
	seen := make(map[core.RoleKey]bool)
	for _, role := range roles {
		if seen[role] {
			return core.TeamRosterError("duplicate role in key rotation")
		}
		seen[role] = true
		if _, ok := keysPre[role]; !ok {
			return core.TeamRosterError("can't rotate a role that has no key")
		}
		if doerInfo.Role.LessThan(role) {
			return core.TeamRosterError("doer role insufficient for key rotation")
		}
	}
	return nil
}

// LinkKind is what the metadata of a team link says it is.
type LinkKind struct {
	Leave  bool
	Rotate []core.RoleKey
}

func (k LinkKind) IsLeave() bool  { return k.Leave }
func (k LinkKind) IsRotate() bool { return len(k.Rotate) > 0 }

// opts are the GameplanOpts that replay a link of this kind, or nil for a
// link of the kinds that existed before (whose replay must not change).
func (k LinkKind) opts() *GameplanOpts {
	if !k.IsLeave() && !k.IsRotate() {
		return nil
	}
	return &GameplanOpts{Leave: k.Leave, Rotate: k.Rotate}
}

// ReadLinkKind reads the TeamLeave and TeamRotateKeys entries of a team
// link's metadata. Either one must be the only entry, and a link that makes
// no new keys (a leave) must carry none.
func ReadLinkKind(gc *proto.GroupChange) (*LinkKind, error) {
	var ret LinkKind
	var n int
	for _, md := range gc.Metadata {
		typ, err := md.GetT()
		if err != nil {
			return nil, err
		}
		switch typ {
		case proto.ChangeType_TeamLeave:
			if ret.Leave {
				return nil, core.LinkError("only one leave allowed")
			}
			ret.Leave = true
			n++
		case proto.ChangeType_TeamRotateKeys:
			if ret.IsRotate() {
				return nil, core.LinkError("only one key rotation allowed")
			}
			raw := md.Teamrotatekeys()
			if len(raw) == 0 {
				return nil, core.LinkError("a key rotation needs at least one role")
			}
			for _, r := range raw {
				rk, err := core.ImportRole(r)
				if err != nil {
					return nil, err
				}
				if rk.IsNone() {
					return nil, core.LinkError("can't rotate the NONE role")
				}
				ret.Rotate = append(ret.Rotate, *rk)
			}
			n++
		}
	}
	if n == 0 {
		return &ret, nil
	}
	if n != 1 || len(gc.Metadata) != 1 {
		return nil, core.LinkError("a leave or key rotation must be the link's only metadata")
	}
	if gc.Chainer.Base.Seqno.IsEldest() {
		return nil, core.LinkError("a team's first link can't be a leave or key rotation")
	}
	if gc.Entity.Entity.Type().IsAdHocTeam() {
		return nil, core.TeamError(AdHocTeamImmutableMsg)
	}
	if ret.Leave && len(gc.SharedKeys) != 0 {
		return nil, core.LinkError("a leave can't make new keys")
	}
	return &ret, nil
}

// nextRotationDue is the RotationDue after a link: a leave makes every role
// at or below the leaver's role due (unless it already is), and a new key
// generation of a role makes it no longer due.
func nextRotationDue(
	pre RotationDue,
	keysPre KeyGens,
	leaver *MemberInfo,
	leave *Change,
	sched *KeySchedule,
) RotationDue {
	ret := pre.Clone()
	if leaver != nil && leave != nil {
		for role := range keysPre {
			if !role.LessThanOrEqual(leaver.Role) {
				continue
			}
			if _, already := ret[role]; already {
				continue
			}
			ret[role] = RotationDueInfo{Seqno: leave.Info.Seqno, Time: leave.Info.Time}
		}
	}
	for _, item := range sched.Items {
		if item.NewKeyGen {
			delete(ret, item.Role)
		}
	}
	return ret
}
