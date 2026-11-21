// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"reflect"
	"sort"
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/stretchr/testify/require"
)

func editArgFieldPaths() []string {
	var ret []string
	_ = editTeamArgFields(reflect.ValueOf(rem.EditTeamArg{}), func(path string, _ reflect.Value) error {
		ret = append(ret, path)
		return nil
	})
	sort.Strings(ret)
	return ret
}

func ruleKeys(r map[string]editFieldRule) []string {
	var ret []string
	for k := range r {
		ret = append(ret, k)
	}
	sort.Strings(ret)
	return ret
}

// A field added to rem.EditTeamArg or rem.OffchainBoxData must be given a
// rule for a leave and for a key rotation before these tests pass again
// (until then the server refuses both kinds of link that carry it).
func TestEditFieldRulesNameEveryField(t *testing.T) {
	paths := editArgFieldPaths()
	require.Equal(t, paths, ruleKeys(leaveEditFields))
	require.Equal(t, paths, ruleKeys(rotateEditFields))
}

func leaveArg() rem.EditTeamArg {
	return rem.EditTeamArg{
		Obd: rem.OffchainBoxData{
			Removals: []rem.TeamRemovalAndComm{{}},
		},
	}
}

func TestLeaveCarriesOnlyItsRemoval(t *testing.T) {
	ok := leaveArg()
	require.NoError(t, checkEditFields(&ok, leaveEditFields, "a leave"))

	// Empty, non-nil slices (as a decoder may make them) are still empty.
	ok.Obd.SeedChain = []proto.SeedChainBox{}
	ok.Obd.PtkBoxes.Boxes = []proto.SharedKeyBox{}
	require.NoError(t, checkEditFields(&ok, leaveEditFields, "a leave"))

	bad := map[string]func(a *rem.EditTeamArg){
		"Tok":              func(a *rem.EditTeamArg) { a.Tok = &rem.TeamBearerToken{} },
		"InsLocalPermsFor": func(a *rem.EditTeamArg) { a.InsLocalPermsFor = []proto.PartyID{{}} },
		"PtkBoxes":         func(a *rem.EditTeamArg) { a.Obd.PtkBoxes.Boxes = []proto.SharedKeyBox{{}} },
		"PtkBoxes.Id":      func(a *rem.EditTeamArg) { a.Obd.PtkBoxes.Id[0] = 1 },
		"SeedChain":        func(a *rem.EditTeamArg) { a.Obd.SeedChain = []proto.SeedChainBox{{}} },
		"RemoteMemberViewTokens": func(a *rem.EditTeamArg) {
			a.Obd.RemoteMemberViewTokens = []proto.TeamRemoteMemberViewToken{{}}
		},
		"RemovalKeys":    func(a *rem.EditTeamArg) { a.Obd.RemovalKeys = []rem.TeamRemovalBoxData{{}} },
		"two removals":   func(a *rem.EditTeamArg) { a.Obd.Removals = append(a.Obd.Removals, rem.TeamRemovalAndComm{}) },
		"no removal":     func(a *rem.EditTeamArg) { a.Obd.Removals = nil },
		"Hepks":          func(a *rem.EditTeamArg) { a.Obd.Hepks.V = []proto.HEPK{{}} },
		"NewKeyOnRotate": func(a *rem.EditTeamArg) { a.Obd.NewKeyOnRotate = proto.EntityID{1} },
	}
	for name, f := range bad {
		a := leaveArg()
		f(&a)
		require.Error(t, checkEditFields(&a, leaveEditFields, "a leave"), name)
	}
}

func TestRotationCarriesNothingAboutMembers(t *testing.T) {
	ok := rem.EditTeamArg{
		Tok: &rem.TeamBearerToken{},
		Obd: rem.OffchainBoxData{
			PtkBoxes:  proto.SharedKeyBoxSet{Boxes: []proto.SharedKeyBox{{}}},
			SeedChain: []proto.SeedChainBox{{}},
			Hepks:     proto.HEPKSet{V: []proto.HEPK{{}}},
		},
	}
	require.NoError(t, checkEditFields(&ok, rotateEditFields, "a key rotation"))

	bad := map[string]func(a *rem.EditTeamArg){
		"InsLocalPermsFor": func(a *rem.EditTeamArg) { a.InsLocalPermsFor = []proto.PartyID{{}} },
		"RemoteMemberViewTokens": func(a *rem.EditTeamArg) {
			a.Obd.RemoteMemberViewTokens = []proto.TeamRemoteMemberViewToken{{}}
		},
		"RemovalKeys":    func(a *rem.EditTeamArg) { a.Obd.RemovalKeys = []rem.TeamRemovalBoxData{{}} },
		"Removals":       func(a *rem.EditTeamArg) { a.Obd.Removals = []rem.TeamRemovalAndComm{{}} },
		"NewKeyOnRotate": func(a *rem.EditTeamArg) { a.Obd.NewKeyOnRotate = proto.EntityID{1} },
	}
	for name, f := range bad {
		a := ok
		f(&a)
		require.Error(t, checkEditFields(&a, rotateEditFields, "a key rotation"), name)
	}
}
