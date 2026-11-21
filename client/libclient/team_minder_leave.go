// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libclient

import (
	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/lib/team"
	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
)

// Leaving a team and the key rotation that follows it (lib/team/leave.go).
//
// TeamLeave posts a TeamLeave link signed by the active user: it takes the
// user out of the team at once (the server stops serving them the team),
// but makes no new keys. RotateKeysIfDue, run by the client of an admin or
// owner who stays, makes them: a TeamRotateKeys link for every role a
// departed member could read.

// TeamLeaveOpts are the hooks of TeamLeave.
type TeamLeaveOpts struct {
	// BeforePost, if set, runs once the leave has passed every check this
	// client can make and right before the link goes to the server, while
	// the user is still a member (an app posts "bob left" here).
	BeforePost func(m MetaContext)

	// TestEditArg, for tests only: changes what is posted, as a modified
	// client could.
	TestEditArg func(arg *rem.EditTeamArg)
}

// TeamLeaveError: the active user can't leave the team this way. Which
// says why, for a caller that maps it to its own words.
type TeamLeaveError struct {
	Which TeamLeaveRefusal
	Msg   string
}

type TeamLeaveRefusal int

const (
	TeamLeaveLastOwner TeamLeaveRefusal = iota + 1
	TeamLeaveNotMember
	TeamLeaveViaTeam
	TeamLeaveOtherHost
	TeamLeaveAdHoc
)

func (e TeamLeaveError) Error() string { return "can't leave team: " + e.Msg }

// TeamLeave takes the active user out of a team of their home server.
func (t *TeamMinder) TeamLeave(
	m MetaContext,
	arg proto.FQTeamParsed,
	opts TeamLeaveOpts,
) error {
	m = m.WithLogTag("team-leave")
	fqt, err := t.ResolveAndReindex(m, team.WrapNamed(arg), &LoadTeamOpts{Refresh: true})
	if err != nil {
		return err
	}
	if fqt == nil {
		return core.TeamNotFoundError{}
	}
	if fqt.Team.IsAdHocTeam() {
		return TeamLeaveError{Which: TeamLeaveAdHoc, Msg: "a conversation's members are fixed"}
	}
	if !fqt.Host.Eq(t.au.HostID()) {
		return TeamLeaveError{Which: TeamLeaveOtherHost, Msg: "the team is on another server"}
	}
	// The removal record goes with the current membership's removal key:
	// after a leave (or a removal) and a new add, the kept one is stale.
	tr, err := t.LoadTeamWithFQTeam(m, *fqt, LoadTeamOpts{Refresh: true, FreshRemovalKey: true})
	if err != nil {
		return err
	}

	l, err := t.prepareLeave(m, tr)
	if err != nil {
		return err
	}
	if opts.BeforePost != nil {
		opts.BeforePost(m)
	}
	err = l.post(m, opts.TestEditArg)
	if err != nil {
		return err
	}
	m.Infow("TeamLeave", "team", *fqt, "seqno", l.seqno, "stage", "posted")

	// The user's own membership chain says "removed", so explorations stop
	// visiting the team at once. Best effort: if it fails, the next
	// exploration finds the team refusing us and fixes it the same way,
	// from the removal posted with the leave.
	ucli, err := t.au.UserClient(m)
	if err == nil {
		var arg *rem.PostGenericLinkArg
		arg, err = t.makeMembershipChainLink(m, nil, proto.NewGenericLinkPayloadWithTeammembership(
			proto.TeamMembershipLink{
				Team:    *fqt,
				SrcRole: l.srcRole,
				State:   proto.NewTeamMembershipDetailsDefault(proto.TeamMembershipLinkState_Removed),
			},
		), nil)
		if err == nil {
			err = ucli.PostGenericLink(m.Ctx(), *arg)
		}
	}
	if err != nil {
		m.Warnw("TeamLeave", "team", *fqt, "stage", "membership chain", "err", err)
	}

	t.forgetTeam(*fqt)
	return nil
}

// forgetTeam drops a team from the minder's index (after a leave), so
// nothing is served from its record until the next exploration.
func (t *TeamMinder) forgetTeam(fqt proto.FQTeam) {
	t.indexMu.Lock()
	defer t.indexMu.Unlock()
	delete(t.loadedTeams, fqt)
	for k, v := range t.teamIndex {
		if v.Eq(fqt) {
			delete(t.teamIndex, k)
		}
	}
}

type teamLeaver struct {
	tm      *TeamMinder
	fqt     proto.FQTeam
	srcRole proto.Role
	link    *core.MakeLinkResBase
	removal rem.TeamRemovalAndComm
	seqno   proto.Seqno
}

// prepareLeave checks the leave against the loaded team and builds the
// link and the removal that go with it.
func (t *TeamMinder) prepareLeave(m MetaContext, tr *TeamRecord) (*teamLeaver, error) {
	tr.Lock()
	defer tr.Unlock()

	if tr.member == nil || tr.ldr == nil || tr.ldr.rosterPost == nil {
		return nil, core.NewConnectError("leaving a team needs the server",
			core.InternalError("team loaded from an offline snapshot"))
	}
	if !IsUser(tr.member) {
		return nil, TeamLeaveError{Which: TeamLeaveViaTeam, Msg: "you are a member through another team"}
	}
	tw := tr.tw
	fqt := tw.FQTeam()
	party := tr.member.FQParty()
	srcRole := tr.member.SrcRole()

	mbmr, err := tw.GetMember(party, srcRole)
	if err != nil {
		return nil, err
	}
	if mbmr == nil {
		return nil, TeamLeaveError{Which: TeamLeaveNotMember, Msg: "you are not a member"}
	}
	typ, err := mbmr.Mr.Member.Keys.GetT()
	if err != nil {
		return nil, err
	}
	if typ != proto.MemberKeysType_Team {
		return nil, core.InternalError("expected team keys")
	}
	signKey, err := tr.member.PrivateKeyAt(m, mbmr.Mr.Member.Keys.Team().Gen)
	if err != nil {
		return nil, err
	}
	if signKey == nil {
		return nil, core.KeyNotFoundError{Which: "member key"}
	}
	vk, err := signKey.EntityPublic()
	if err != nil {
		return nil, err
	}

	ko := proto.KeyOwner{Party: party.Party, SrcRole: srcRole}
	change := proto.MemberRole{
		DstRole: proto.NewRoleDefault(proto.RoleType_NONE),
		Member: proto.Member{
			Id:      party.FQEntity().AtHost(fqt.Host),
			SrcRole: srcRole,
			Keys:    proto.NewMemberKeysWithNone(),
		},
	}

	// The same rules the server and every replaying client apply.
	_, _, err = tr.ldr.rosterPost.Gameplan(
		ko, fqt.Host,
		[]proto.MemberRoleSeqno{{Mr: change}},
		vk.GetEntityID(),
		&team.GameplanOpts{Leave: true},
	)
	if err != nil {
		if rerr, ok := err.(core.TeamRosterError); ok && string(rerr) == team.LastOwnerLeaveMsg {
			return nil, TeamLeaveError{Which: TeamLeaveLastOwner, Msg: "you are the last owner"}
		}
		return nil, err
	}

	rk := tw.RemovalKey()
	if rk == nil {
		return nil, core.KeyNotFoundError{Which: "team removal key"}
	}

	ma, err := t.au.MerkleAgent(m)
	if err != nil {
		return nil, err
	}
	root, err := ma.GetLatestTreeRootFromServer(m.Ctx())
	if err != nil {
		return nil, err
	}

	seqno := tw.Prot().Tail.Base.Seqno + 1
	link, err := team.MakeTeamLink(
		fqt.Host,
		fqt.Team,
		ko,
		signKey,
		[]proto.MemberRole{change},
		nil,
		seqno,
		tw.Prot().LastHash,
		root,
		[]proto.ChangeMetadata{proto.NewChangeMetadataWithTeamleave()},
	)
	if err != nil {
		return nil, err
	}

	// The removal record a removed member's client checks (TeamLoader.
	// VerifyRemoval) is MACed with the member's removal key, which the
	// member holds too. Here the member removes itself, and says so.
	mp := rem.TeamRemovalMACPayload{
		Team:    fqt,
		Member:  party,
		SrcRole: srcRole,
		Admin:   party,
		Root:    root,
		Tm:      proto.Now(),
	}
	hmk := proto.HMACKey(*rk)
	mac, err := core.Hmac(&mp, &hmk)
	if err != nil {
		return nil, err
	}
	comm, err := core.ComputeKeyCommitment(rk)
	if err != nil {
		return nil, err
	}

	return &teamLeaver{
		tm:      t,
		fqt:     fqt,
		srcRole: srcRole,
		link:    link,
		seqno:   seqno,
		removal: rem.TeamRemovalAndComm{
			Rm:   rem.TeamRemoval{Mac: *mac, Payload: mp},
			Comm: *comm,
		},
	}, nil
}

func (l *teamLeaver) post(m MetaContext, testEdit func(*rem.EditTeamArg)) error {
	cli, err := l.tm.au.TeamAdminClient(m)
	if err != nil {
		return err
	}
	arg := rem.EditTeamArg{
		Link:             *l.link.Link,
		NextTreeLocation: *l.link.NextTreeLocation,
		Obd: rem.OffchainBoxData{
			Removals: []rem.TeamRemovalAndComm{l.removal},
		},
	}
	if testEdit != nil {
		testEdit(&arg)
	}
	_, err = cli.EditTeam(m.Ctx(), arg)
	return err
}

// TeamRotationRes is what RotateKeysIfDue did for one team.
type TeamRotationRes struct {
	Team proto.FQTeam
	// Rotated: the roles this call made new keys for.
	Rotated []core.RoleKey
	// StillDue: due roles above this client's role (an owner left and only
	// an owner can rotate the owner key), and when the first leave was.
	StillDue team.RotationDue
}

// RotationDue is what a load of the team says is due, refreshed from the
// server. An empty result means nothing is due.
func (t *TeamMinder) RotationDue(m MetaContext, fqt proto.FQTeam) (team.RotationDue, error) {
	tr, err := t.LoadTeamWithFQTeam(m, fqt, LoadTeamOpts{Refresh: true})
	if err != nil {
		return nil, err
	}
	return tr.Tw().RotationDue()
}

// RotateKeysIfDue makes new keys for every role of the team that a member
// who left could read and this client may rotate (an admin: up to admin;
// an owner: all). It does nothing when nothing is due. Two admins racing
// make one rotation: the second one's link is refused (its seqno is taken)
// and, reloaded, the team is no longer due.
func (t *TeamMinder) RotateKeysIfDue(m MetaContext, fqt proto.FQTeam) (*TeamRotationRes, error) {
	m = m.WithLogTag("team-rotate")
	ret := &TeamRotationRes{Team: fqt}

	tr, err := t.LoadTeamWithFQTeam(m, fqt, LoadTeamOpts{Refresh: true})
	if err != nil {
		return nil, err
	}
	due, err := tr.Tw().RotationDue()
	if err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return ret, nil
	}

	tr.Lock()
	actor, err := tr.actorRoleLocked()
	tr.Unlock()
	if err != nil {
		return nil, err
	}
	var roles []core.RoleKey
	ret.StillDue = make(team.RotationDue)
	for rk, info := range due {
		if actor.IsAdminOrAbove() && rk.LessThanOrEqual(*actor) {
			roles = append(roles, rk)
		} else {
			ret.StillDue[rk] = info
		}
	}
	if len(roles) == 0 {
		return ret, nil
	}
	sortRoles(roles)

	tok, cli, tr, err := t.adminTokenAndClient(m, fqt, LoadTeamOpts{Refresh: true, Members: MemberLoadFull})
	if err != nil {
		return nil, err
	}
	cfg, err := t.loadConfig(m, cli)
	if err != nil {
		return nil, err
	}

	exported := make([]proto.Role, len(roles))
	for i, r := range roles {
		exported[i] = r.Export()
	}

	err = func() error {
		tr.Lock()
		defer tr.Unlock()
		// Due again under the lock, on the record the token was made for.
		now, err := tr.tw.RotationDue()
		if err != nil {
			return err
		}
		for _, r := range roles {
			if _, ok := now[r]; !ok {
				return errRotationRaced
			}
		}
		editor := TeamEditor{
			tl:       tr.ldr,
			tw:       tr.tw,
			id:       tr.ldr.TeamID(),
			tok:      tok,
			pre:      tr.ldr.rosterPost,
			cp:       tr.member,
			cfg:      cfg,
			hepks:    core.NewHEPKSet(),
			rotate:   roles,
			cmd:      []proto.ChangeMetadata{proto.NewChangeMetadataWithTeamrotatekeys(exported)},
			testOpts: t.teamEditorTestOpts(),
		}
		return editor.Run(m)
	}()
	if err == errRotationRaced {
		m.Infow("RotateKeysIfDue", "team", fqt, "note", "someone else rotated first")
		return ret, nil
	}
	if err != nil {
		return nil, err
	}
	ret.Rotated = roles
	m.Infow("RotateKeysIfDue", "team", fqt, "rotated", len(roles))

	// Not loaded again here: the new link is only in the merkle tree after
	// the server's next batch, and a load before that fails. The next load
	// of the team (the PLC refreshes every TeamCacheTimeout) picks up the
	// new keys, as after any admin's change.
	return ret, nil
}

var errRotationRaced = core.TeamRaceError("rotation no longer due")

func sortRoles(r []core.RoleKey) {
	for i := range r {
		for j := i + 1; j < len(r); j++ {
			if r[j].Cmp(r[i]) < 0 {
				r[i], r[j] = r[j], r[i]
			}
		}
	}
}
