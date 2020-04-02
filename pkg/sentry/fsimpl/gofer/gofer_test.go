// Copyright 2020 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package gofer

import (
	"testing"

	"gvisor.dev/gvisor/pkg/p9"
	"gvisor.dev/gvisor/pkg/sentry/contexttest"
)

func TestDestroyIdempotent(t *testing.T) {
	fs := filesystem{
		dentries: make(map[*dentry]struct{}),
	}

	ctx := contexttest.Context(t)
	attr := &p9.Attr{
		Mode: p9.ModeRegular,
	}
	mask := p9.AttrMask{
		Mode: true,
		Size: true,
	}
	parent, err := fs.newDentry(ctx, p9file{}, p9.QID{}, mask, attr)
	if err != nil {
		t.Fatalf("fs.newDentry(): %v", err)
	}

	child, err := fs.newDentry(ctx, p9file{}, p9.QID{}, mask, attr)
	if err != nil {
		t.Fatalf("fs.newDentry(): %v", err)
	}
	parent.IncRef() // reference held by child on its parent.
	parent.vfsd.InsertChild(&child.vfsd, "child")

	child.destroyLocked()
	child.destroyLocked()
	child.destroyLocked()
}
