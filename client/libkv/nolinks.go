// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libkv

import (
	proto "github.com/foks-proj/go-foks/proto/lib"
)

// LinkInPathError is the refusal of a walk that would have to follow a
// symlink, by a Minder that was told not to (SetNoLinks). Path is the link.
// It never crosses an RPC: it is made and read in the process that owns the
// Minder.
type LinkInPathError struct {
	Path proto.KVPath
}

func (e LinkInPathError) Error() string {
	return "path goes through a symlink: " + string(e.Path)
}

// SetNoLinks makes every path walk of this Minder refuse to follow a symlink,
// in any component, with LinkInPathError. What works on a link itself still
// does: Readlink, Stat with NoFollow, Unlink, and Mv with the link as its
// source. It is for an embedder that confines callers to a part of the tree
// by the text of their paths, which a link would lead out of. Set it before
// the Minder is used.
func (k *Minder) SetNoLinks(on bool) {
	k.Lock()
	defer k.Unlock()
	k.noLinks = on
}
