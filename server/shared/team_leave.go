// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"errors"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/jackc/pgx/v5"
)

// RevokeTeamBearerTokensOfHolder ends every team bearer token a local user
// holds for the team (inert or active), as when that user leaves the team
// (lib/team/leave.go): a leave makes no new keys, so without this a token
// made while a member -- an admin's, say -- would stay good until the next
// key rotation.
func RevokeTeamBearerTokensOfHolder(
	m MetaContext,
	tx pgx.Tx,
	teamID proto.TeamID,
	holder proto.UID,
) error {
	_, err := tx.Exec(m.Ctx(),
		`UPDATE team_bearer_tokens
		 SET state=$1
		 WHERE short_host_id=$2 AND team_id=$3
		 AND holder_uid=$4 AND holder_host_id=$5
		 AND state IN ($6, $7)`,
		team.BearerTokenStateRevoked,
		m.ShortHostID().ExportToDB(),
		teamID.ExportToDB(),
		holder.ExportToDB(),
		LocalHost,
		team.BearerTokenStateInert,
		team.BearerTokenStateActive,
	)
	return err
}

// AssertMayHoldTeamKey: a signature by a team key proves only that the
// signer was once given that key, not that they may still use it. A member
// who leaves keeps every key they were boxed (a leave makes no new keys; an
// admin's device rotates them later), so the server must not take such a
// signature alone as the team's word.
//
// Refused: a holder who was boxed the current generation of the role's key
// directly (so they were a direct member when it was made, or were given it
// later at that generation) and has no active direct roster row at that
// role or above any more. That is exactly a member who left since the key
// was made, including one who left and was added back at a lower role. A
// removal or demotion by an admin makes the new keys in the same link, so
// it never leaves a holder in this state, and a rotation ends it.
//
// A holder with no direct box at this generation gets the key another way,
// through a member team, and is judged as before (the signature). So an
// admin who left but is still an admin through a member team is refused
// until the rotation; that is the safe side.
func AssertMayHoldTeamKey(
	m MetaContext,
	q Querier,
	teamID proto.TeamID,
	holder proto.UID,
	holderHost proto.HostID,
	role proto.Role,
	gen proto.Generation,
) error {
	rk, err := core.ImportRole(role)
	if err != nil {
		return err
	}
	hosts := holderHostsInDB(m, holderHost)

	rows, err := q.Query(m.Ctx(),
		`SELECT dst_role_type, dst_viz_level
		 FROM team_members
		 WHERE short_host_id=$1 AND team_id=$2 AND member_id=$3
		 AND member_host_id = ANY($4) AND active=TRUE`,
		m.ShortHostID().ExportToDB(),
		teamID.ExportToDB(),
		holder.ExportToDB(),
		hosts,
	)
	if err != nil {
		return err
	}
	var activeAtRole bool
	for rows.Next() {
		var typ, lev int
		err = rows.Scan(&typ, &lev)
		if err != nil {
			rows.Close()
			return err
		}
		drk, err := core.ImportRoleKeyFromDB(typ, lev)
		if err != nil {
			rows.Close()
			return err
		}
		if rk.LessThanOrEqual(*drk) {
			activeAtRole = true
		}
	}
	rows.Close()
	err = rows.Err()
	if err != nil {
		return err
	}
	if activeAtRole {
		return nil
	}

	var one int
	err = q.QueryRow(m.Ctx(),
		`SELECT 1 FROM shared_key_boxes
		 WHERE short_host_id=$1 AND entity_id=$2
		 AND target_entity_id=$3 AND target_host_id = ANY($4)
		 AND gen=$5 AND role_type=$6 AND viz_level=$7
		 LIMIT 1`,
		m.ShortHostID().ExportToDB(),
		teamID.ExportToDB(),
		holder.ExportToDB(),
		hosts,
		int(gen),
		int(rk.Typ),
		int(rk.Lev),
	).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return core.PermissionError("no longer a member of the team at this role")
}

// assertMayActAsMemberTeam: a VO bearer token for a member that is itself a
// team on this host is signed with that member team's key. The connection's
// user must not be someone who left the member team and still holds its key
// (AssertMayHoldTeamKey on the member team). A user member, or a member team
// on another host, is judged by the signature alone, as before.
func assertMayActAsMemberTeam(
	m MetaContext,
	q Querier,
	req rem.TeamVOBearerTokenReq,
) error {
	if !req.Member.Host.Eq(m.HostID().Id) {
		return nil
	}
	_, tid, err := req.Member.Party.Select()
	if err != nil {
		return err
	}
	if tid == nil {
		return nil
	}
	if m.UID().IsZero() {
		return core.NeedLoginError{}
	}
	return AssertMayHoldTeamKey(m, q, *tid, m.UID(), m.HostID().Id, req.SrcRole, req.Gen)
}

// holderHostsInDB: the ways a member's host is written in team_members and
// shared_key_boxes (a local member as LocalHost, or the host ID written out
// by a client that names it).
func holderHostsInDB(m MetaContext, h proto.HostID) [][]byte {
	if m.HostID().Id.Eq(h) {
		return [][]byte{LocalHost, h.ExportToDB()}
	}
	return [][]byte{h.ExportToDB()}
}
