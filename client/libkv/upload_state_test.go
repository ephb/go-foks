// Copyright (c) 2025 ne43, Inc.
// Licensed under the MIT License. See LICENSE in the project root for details.

package libkv

import (
	"sync"
	"testing"

	proto "github.com/foks-proj/go-foks/proto/lib"
)

// One party's uploads run side by side through one Minder (two git pushes,
// two large files): PutFileFirst notes an upload while PutFileChunk looks
// another one up and drops a finished one. With a bare map this is a fatal
// "concurrent map writes" that no recover catches.
func TestUploadStatesSideBySide(t *testing.T) {
	c := &caches{upload: make(map[proto.FileID]*fileUploadState)}
	const workers, rounds = 16, 2000
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				var id proto.FileID
				id[0], id[1], id[2] = byte(w), byte(i), byte(i>>8)
				// PutFileFirst, not final.
				c.putUpload(id, &fileUploadState{off: proto.Offset(i)})
				// PutFileChunk.
				us := c.getUpload(id)
				if us == nil || us.off != proto.Offset(i) {
					t.Errorf("upload %d/%d: got %+v", w, i, us)
					return
				}
				// Its final chunk.
				c.dropUpload(id)
				if c.getUpload(id) != nil {
					t.Errorf("upload %d/%d stayed after its last chunk", w, i)
					return
				}
			}
		}(w)
	}
	wg.Wait()
	if n := len(c.upload); n != 0 {
		t.Fatalf("%d upload states left", n)
	}
}
