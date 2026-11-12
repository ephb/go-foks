// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package main

import (
	"fmt"

	"github.com/foks-proj/go-foks/lib/core"
	"github.com/foks-proj/go-foks/proto/rem"
	"github.com/foks-proj/go-foks/server/shared"
	"github.com/spf13/cobra"
)

// InviteCodeCmd lists, mints and disables multi-use invite codes for the
// primary host, the same kind `standup` makes. Handy for a standalone
// server, where the admin web app is not running.
type InviteCodeCmd struct {
	CLIAppBase
	mint    int
	code    string
	disable string
}

func (c *InviteCodeCmd) CobraConfig() *cobra.Command {
	ret := &cobra.Command{
		Use:   "invite-code",
		Short: "List, make or disable multi-use invite codes for this host",
		Long: `List, make or disable multi-use invite codes for this host.
With no flags, lists all codes with their use counts. With --new N, makes N
random codes and prints them one per line. With --code, makes that exact code.
With --disable, stops a code from being used for new signups.`,
		SilenceUsage: true,
	}
	ret.Flags().IntVar(&c.mint, "new", 0, "make this many new random codes")
	ret.Flags().StringVar(&c.code, "code", "", "make this exact code")
	ret.Flags().StringVar(&c.disable, "disable", "", "disable this code")
	return ret
}

func (c *InviteCodeCmd) CheckArgs(args []string) error {
	if len(args) != 0 {
		return core.BadArgsError("no args allowed")
	}
	n := 0
	if c.mint != 0 {
		n++
	}
	if c.code != "" {
		n++
	}
	if c.disable != "" {
		n++
	}
	if n > 1 {
		return core.BadArgsError("use only one of --new, --code, --disable")
	}
	if c.mint < 0 || c.mint > 100 {
		return core.BadArgsError("--new must be between 1 and 100")
	}
	return nil
}

func (c *InviteCodeCmd) Run(m shared.MetaContext) error {
	switch {
	case c.disable != "":
		return shared.DisableMultiUseInviteCode(m, rem.MultiUseInviteCode(c.disable))
	case c.code != "":
		err := shared.InsertMultiuseInviteCode(m, rem.MultiUseInviteCode(c.code))
		if err != nil {
			return err
		}
		fmt.Println(c.code)
		return nil
	case c.mint > 0:
		for i := 0; i < c.mint; i++ {
			code, err := core.RandomBase36String(8)
			if err != nil {
				return err
			}
			err = shared.InsertMultiuseInviteCode(m, rem.MultiUseInviteCode(code))
			if err != nil {
				return err
			}
			fmt.Println(code)
		}
		return nil
	default:
		codes, err := shared.LoadAllMultiuseInviteCodes(m)
		if err != nil {
			return err
		}
		for _, ic := range codes {
			state := "valid"
			if !ic.Valid {
				state = "disabled"
			}
			fmt.Printf("%s\t%s\t%d uses\n", ic.Code, state, ic.Uses)
		}
		return nil
	}
}

func (c *InviteCodeCmd) SetGlobalContext(g *shared.GlobalContext) {}

var _ shared.CLIApp = (*InviteCodeCmd)(nil)

func init() {
	AddCmd(&InviteCodeCmd{})
}
