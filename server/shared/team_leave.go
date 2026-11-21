// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package shared

import (
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
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
