// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package engine

import (
	"fmt"
	"reflect"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/rem"
)

// What a TeamLeave or a TeamRotateKeys link may carry besides the link.
//
// EditTeam acts on every field of its argument (rem.EditTeamArg and its
// OffchainBoxData): view tokens for remote members, key boxes, the seed
// chain, removal keys, removals, local view permissions. For an admin's
// ordinary edit the link's rules and the key schedule bound most of them.
// A leave is posted by a member who need not be an admin, so it may carry
// nothing but its own removal; a rotation adds and removes nobody, so it
// may carry nothing about members. The tables below name every field, and
// a field that is not named is refused, so a field added to the protocol
// later is refused for these two kinds of link until someone decides
// (TestEditFieldRulesNameEveryField fails until then).

type editFieldRule int

const (
	// editFieldAny: the link's own data, checked by the link's rules and
	// the key schedule.
	editFieldAny editFieldRule = iota + 1
	// editFieldEmpty: must be absent.
	editFieldEmpty
	// editFieldOne: exactly one element (a leave's own removal record).
	editFieldOne
)

// leaveEditFields: a leave is the link, where it goes in the tree, and
// the leaver's own removal record. No token (checkLeave), no keys, no
// hepks, no view tokens or permissions.
var leaveEditFields = map[string]editFieldRule{
	"Link":                       editFieldAny,
	"NextTreeLocation":           editFieldAny,
	"Tok":                        editFieldEmpty,
	"InsLocalPermsFor":           editFieldEmpty,
	"Obd.PtkBoxes":               editFieldEmpty,
	"Obd.SeedChain":              editFieldEmpty,
	"Obd.RemoteMemberViewTokens": editFieldEmpty,
	"Obd.RemovalKeys":            editFieldEmpty,
	"Obd.Removals":               editFieldOne,
	"Obd.Hepks":                  editFieldEmpty,
	"Obd.NewKeyOnRotate":         editFieldEmpty,
}

// rotateEditFields: a rotation is the link, the new keys (boxes, seed
// chain, the hepks they are boxed for) and the admin's token. Nothing that
// belongs to adding or removing a member.
var rotateEditFields = map[string]editFieldRule{
	"Link":                       editFieldAny,
	"NextTreeLocation":           editFieldAny,
	"Tok":                        editFieldAny,
	"InsLocalPermsFor":           editFieldEmpty,
	"Obd.PtkBoxes":               editFieldAny,
	"Obd.SeedChain":              editFieldAny,
	"Obd.RemoteMemberViewTokens": editFieldEmpty,
	"Obd.RemovalKeys":            editFieldEmpty,
	"Obd.Removals":               editFieldEmpty,
	"Obd.Hepks":                  editFieldAny,
	"Obd.NewKeyOnRotate":         editFieldEmpty,
}

// editTeamArgFields: the field paths of rem.EditTeamArg, with the fields of
// Obd (rem.OffchainBoxData) one level down.
func editTeamArgFields(v reflect.Value, f func(path string, fv reflect.Value) error) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		name := t.Field(i).Name
		fv := v.Field(i)
		if name == "Obd" {
			ot := fv.Type()
			for j := 0; j < ot.NumField(); j++ {
				err := f("Obd."+ot.Field(j).Name, fv.Field(j))
				if err != nil {
					return err
				}
			}
			continue
		}
		err := f(name, fv)
		if err != nil {
			return err
		}
	}
	return nil
}

// fieldIsEmpty: nil, a zero value, an empty slice or map, or a struct
// whose fields are all empty (a box set with no boxes decodes as one).
func fieldIsEmpty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	case reflect.Ptr, reflect.Interface:
		return v.IsNil()
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if !fieldIsEmpty(v.Field(i)) {
				return false
			}
		}
		return true
	default:
		return v.IsZero()
	}
}

func checkEditFields(arg *rem.EditTeamArg, rules map[string]editFieldRule, what string) error {
	return editTeamArgFields(reflect.ValueOf(*arg), func(path string, fv reflect.Value) error {
		rule, ok := rules[path]
		if !ok {
			return core.PermissionError(fmt.Sprintf("%s can't carry %s", what, path))
		}
		switch rule {
		case editFieldAny:
			return nil
		case editFieldEmpty:
			if !fieldIsEmpty(fv) {
				return core.PermissionError(fmt.Sprintf("%s can't carry %s", what, path))
			}
			return nil
		case editFieldOne:
			if fv.Kind() != reflect.Slice || fv.Len() != 1 {
				return core.PermissionError(fmt.Sprintf("%s must carry exactly one of %s", what, path))
			}
			return nil
		default:
			return core.InternalError("unknown edit field rule")
		}
	})
}

// checkLinkKindArg: a leave or a rotation carries only what its kind
// needs (see the tables above). Other links are unchanged.
func (c *teamEditor) checkLinkKindArg() error {
	switch {
	case c.openres.Kind.IsLeave():
		return checkEditFields(&c.arg, leaveEditFields, "a leave")
	case c.openres.Kind.IsRotate():
		return checkEditFields(&c.arg, rotateEditFields, "a key rotation")
	default:
		return nil
	}
}
