// Copyright 2018 The gVisor Authors.
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

package segment

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"
)

const (
	// testSize is the baseline number of elements inserted into sets under
	// test, and is chosen to be large enough to ensure interesting amounts of
	// tree rebalancing.
	//
	// Note that because checkSet is called between each insertion/removal in
	// some tests that use it, tests may be quadratic in testSize.
	testSize = 8000

	// valueOffset is the difference between the value and start of test
	// segments.
	valueOffset = 100000

	// intervalLength is the interval used by random gap tests.
	intervalLength = 10
)

func shuffle(xs []int) {
	rand.Shuffle(len(xs), func(i, j int) { xs[i], xs[j] = xs[j], xs[i] })
}

func randIntervalPermutation(size int) []int {
	p := make([]int, size)
	for i := range p {
		p[i] = intervalLength * i
	}
	shuffle(p)
	return p
}

// validate can be passed to Check.
func validate(nr int, r Range, v int) error {
	if got, want := v, r.Start+valueOffset; got != want {
		return fmt.Errorf("segment %d has key %d, value %d (expected %d)", nr, r.Start, got, want)
	}
	return nil
}

// checkSetMaxGap returns an error if maxGap inside all nodes of s is not well
// maintained.
func checkSetMaxGap(s *gapSet) error {
	n := s.root
	return checkNodeMaxGap(&n)
}

// checkNodeMaxGap returns an error if maxGap inside the subtree rooted by n is
// not well maintained.
func checkNodeMaxGap(n *gapnode) error {
	var max int
	if !n.hasChildren {
		max = n.calculateMaxGapLeaf()
	} else {
		for i := 0; i <= n.nrSegments; i++ {
			child := n.children[i]
			if err := checkNodeMaxGap(child); err != nil {
				return err
			}
			if temp := child.maxGap.Get(); i == 0 || temp > max {
				max = temp
			}
		}
	}
	if max != n.maxGap.Get() {
		return fmt.Errorf("maxGap wrong in node\n%vexpected: %d got: %d", n, max, n.maxGap)
	}
	return nil
}

func TestAddRandom(t *testing.T) {
	var s Set
	order := rand.Perm(testSize)
	var nrInsertions int
	for i, j := range order {
		s.InsertWithoutMergingRange(Range{j, j + 1}, j+valueOffset)
		nrInsertions++
		if err := s.segmentTestCheck(nrInsertions, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
	}
	if got, want := s.countSegments(), nrInsertions; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Insertion order: %v", order[:nrInsertions])
		t.Logf("Set contents:\n%v", &s)
	}
}

func TestRemoveRandom(t *testing.T) {
	var s Set
	for i := 0; i < testSize; i++ {
		s.InsertWithoutMergingRange(Range{i, i + 1}, i+valueOffset)
	}
	order := rand.Perm(testSize)
	var nrRemovals int
	for i, j := range order {
		seg := s.FindSegment(j)
		if !seg.Ok() {
			t.Errorf("Iteration %d: failed to find segment with key %d", i, j)
			break
		}
		s.Remove(seg)
		nrRemovals++
		if err := s.segmentTestCheck(testSize-nrRemovals, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
	}
	if got, want := s.countSegments(), testSize-nrRemovals; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Removal order: %v", order[:nrRemovals])
		t.Logf("Set contents:\n%v", &s)
		t.FailNow()
	}
}

func TestMaxGapAddRandom(t *testing.T) {
	var s gapSet
	order := rand.Perm(testSize)
	var nrInsertions int
	for i, j := range order {
		s.InsertWithoutMergingRange(Range{j, j + 1}, j+valueOffset)
		nrInsertions++
		if err := s.segmentTestCheck(nrInsertions, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When inserting %d: %v", j, err)
			break
		}
	}
	if got, want := s.countSegments(), nrInsertions; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Insertion order: %v", order[:nrInsertions])
		t.Logf("Set contents:\n%v", &s)
	}
}

func TestMaxGapAddRandomWithRandomInterval(t *testing.T) {
	var s gapSet
	order := randIntervalPermutation(testSize)
	var nrInsertions int
	for i, j := range order {
		s.InsertWithoutMergingRange(Range{j, j + rand.Intn(intervalLength-1) + 1}, j+valueOffset)
		nrInsertions++
		if err := s.segmentTestCheck(nrInsertions, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When inserting %d: %v", j, err)
			break
		}
	}
	if got, want := s.countSegments(), nrInsertions; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Insertion order: %v", order[:nrInsertions])
		t.Logf("Set contents:\n%v", &s)
	}
}

func TestMaxGapAddRandomWithMerge(t *testing.T) {
	var s gapSet
	order := randIntervalPermutation(testSize)
	for _, j := range order {
		s.InsertRange(Range{j, j + intervalLength}, 0)
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When inserting %d: %v", j, err)
			break
		}
	}
	if got, want := s.countSegments(), 1; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Insertion order: %v", order)
		t.Logf("Set contents:\n%v", &s)
	}
}

func TestMaxGapRemoveRandom(t *testing.T) {
	var s gapSet
	for i := 0; i < testSize; i++ {
		s.InsertWithoutMergingRange(Range{i, i + 1}, i+valueOffset)
	}
	order := rand.Perm(testSize)
	var nrRemovals int
	for i, j := range order {
		seg := s.FindSegment(j)
		if !seg.Ok() {
			t.Errorf("Iteration %d: failed to find segment with key %d", i, j)
			break
		}
		temprange := seg.Range()
		s.Remove(seg)
		nrRemovals++
		if err := s.segmentTestCheck(testSize-nrRemovals, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When removing %v: %v", temprange, err)
			break
		}
	}
	if got, want := s.countSegments(), testSize-nrRemovals; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Removal order: %v", order[:nrRemovals])
		t.Logf("Set contents:\n%v", &s)
		t.FailNow()
	}
}

func TestMaxGapRemoveHalfRandom(t *testing.T) {
	var s gapSet
	for i := 0; i < testSize; i++ {
		s.InsertWithoutMergingRange(Range{intervalLength * i, intervalLength*i + rand.Intn(intervalLength-1) + 1}, intervalLength*i+valueOffset)
	}
	order := randIntervalPermutation(testSize)
	order = order[:testSize/2]
	var nrRemovals int
	for i, j := range order {
		seg := s.FindSegment(j)
		if !seg.Ok() {
			t.Errorf("Iteration %d: failed to find segment with key %d", i, j)
			break
		}
		temprange := seg.Range()
		s.Remove(seg)
		nrRemovals++
		if err := s.segmentTestCheck(testSize-nrRemovals, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When removing %v: %v", temprange, err)
			break
		}
	}
	if got, want := s.countSegments(), testSize-nrRemovals; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Removal order: %v", order[:nrRemovals])
		t.Logf("Set contents:\n%v", &s)
		t.FailNow()
	}
}

func TestMaxGapRemoveHalfRandomWithMerge(t *testing.T) {
	var s gapSet
	s.InsertRange(Range{0, intervalLength * testSize}, 0)
	order := randIntervalPermutation(testSize)
	order = order[:testSize/2]
	var nrRemovals int
	for _, j := range order {
		temprange := Range{j, j + intervalLength}
		s.RemoveFullRange(temprange)
		nrRemovals++
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When removing %v: %v", temprange, err)
			break
		}
	}
	if t.Failed() {
		t.Logf("Removal order: %v", order[:nrRemovals])
		t.Logf("Set contents:\n%v", &s)
		t.FailNow()
	}
}

func TestNextLargeEnoughGap(t *testing.T) {
	var s gapSet
	order := randIntervalPermutation(testSize * 2)
	order = order[:testSize]
	for _, j := range order {
		s.InsertRange(Range{j, j + rand.Intn(intervalLength-1) + 1}, j+valueOffset)
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When inserting %d: %v", j, err)
			break
		}
	}
	shuffle(order)
	order = order[:testSize/2]
	for _, j := range order {
		seg := s.FindSegment(j)
		if !seg.Ok() {
			continue
		}
		temprange := seg.Range()
		s.Remove(seg)
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When removing %v: %v", temprange, err)
			break
		}
	}
	minSize := 7
	var gapArr1 []int
	for gap := s.LowerBoundGap(0).NextLargeEnoughGap(minSize); gap.Ok(); gap = gap.NextLargeEnoughGap(minSize) {
		if gap.Range().Length() < minSize {
			t.Errorf("NextLargeEnoughGap wrong, gap %v has length %d, wanted %d", gap.Range(), gap.Range().Length(), minSize)
		} else {
			gapArr1 = append(gapArr1, gap.Range().Start)
		}
	}
	var gapArr2 []int
	for gap := s.LowerBoundGap(0).NextGap(); gap.Ok(); gap = gap.NextGap() {
		if gap.Range().Length() >= minSize {
			gapArr2 = append(gapArr2, gap.Range().Start)
		}
	}

	if !slices.Equal(gapArr2, gapArr1) {
		t.Errorf("Search result not correct, got: %v, wanted: %v", gapArr1, gapArr2)
	}
	if t.Failed() {
		t.Logf("Set contents:\n%v", &s)
		t.FailNow()
	}
}

func TestPrevLargeEnoughGap(t *testing.T) {
	var s gapSet
	order := randIntervalPermutation(testSize * 2)
	order = order[:testSize]
	for _, j := range order {
		s.InsertRange(Range{j, j + rand.Intn(intervalLength-1) + 1}, j+valueOffset)
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When inserting %d: %v", j, err)
			break
		}
	}
	end := s.LastSegment().End()
	shuffle(order)
	order = order[:testSize/2]
	for _, j := range order {
		seg := s.FindSegment(j)
		if !seg.Ok() {
			continue
		}
		temprange := seg.Range()
		s.Remove(seg)
		if err := checkSetMaxGap(&s); err != nil {
			t.Errorf("When removing %v: %v", temprange, err)
			break
		}
	}
	minSize := 7
	var gapArr1 []int
	for gap := s.UpperBoundGap(end + intervalLength).PrevLargeEnoughGap(minSize); gap.Ok(); gap = gap.PrevLargeEnoughGap(minSize) {
		if gap.Range().Length() < minSize {
			t.Errorf("PrevLargeEnoughGap wrong, gap length %d, wanted %d", gap.Range().Length(), minSize)
		} else {
			gapArr1 = append(gapArr1, gap.Range().Start)
		}
	}
	var gapArr2 []int
	for gap := s.UpperBoundGap(end + intervalLength).PrevGap(); gap.Ok(); gap = gap.PrevGap() {
		if gap.Range().Length() >= minSize {
			gapArr2 = append(gapArr2, gap.Range().Start)
		}
	}
	if !slices.Equal(gapArr2, gapArr1) {
		t.Errorf("Search result not correct, got: %v, wanted: %v", gapArr1, gapArr2)
	}
	if t.Failed() {
		t.Logf("Set contents:\n%v", &s)
		t.FailNow()
	}
}

func TestAddSequentialAdjacent(t *testing.T) {
	var s Set
	var nrInsertions int
	for i := 0; i < testSize; i++ {
		s.InsertWithoutMergingRange(Range{i, i + 1}, i+valueOffset)
		nrInsertions++
		if err := s.segmentTestCheck(nrInsertions, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
	}
	if got, want := s.countSegments(), nrInsertions; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Set contents:\n%v", &s)
	}

	first := s.FirstSegment()
	gotSeg, gotGap := first.PrevNonEmpty()
	if wantGap := s.FirstGap(); gotSeg.Ok() || gotGap != wantGap {
		t.Errorf("FirstSegment().PrevNonEmpty(): got (%v, %v), wanted (<terminal iterator>, %v)", gotSeg, gotGap, wantGap)
	}
	gotSeg, gotGap = first.NextNonEmpty()
	if wantSeg := first.NextSegment(); gotSeg != wantSeg || gotGap.Ok() {
		t.Errorf("FirstSegment().NextNonEmpty(): got (%v, %v), wanted (%v, <terminal iterator>)", gotSeg, gotGap, wantSeg)
	}

	last := s.LastSegment()
	gotSeg, gotGap = last.PrevNonEmpty()
	if wantSeg := last.PrevSegment(); gotSeg != wantSeg || gotGap.Ok() {
		t.Errorf("LastSegment().PrevNonEmpty(): got (%v, %v), wanted (%v, <terminal iterator>)", gotSeg, gotGap, wantSeg)
	}
	gotSeg, gotGap = last.NextNonEmpty()
	if wantGap := s.LastGap(); gotSeg.Ok() || gotGap != wantGap {
		t.Errorf("LastSegment().NextNonEmpty(): got (%v, %v), wanted (<terminal iterator>, %v)", gotSeg, gotGap, wantGap)
	}

	for seg := first.NextSegment(); seg != last; seg = seg.NextSegment() {
		gotSeg, gotGap = seg.PrevNonEmpty()
		if wantSeg := seg.PrevSegment(); gotSeg != wantSeg || gotGap.Ok() {
			t.Errorf("%v.PrevNonEmpty(): got (%v, %v), wanted (%v, <terminal iterator>)", seg, gotSeg, gotGap, wantSeg)
		}
		gotSeg, gotGap = seg.NextNonEmpty()
		if wantSeg := seg.NextSegment(); gotSeg != wantSeg || gotGap.Ok() {
			t.Errorf("%v.NextNonEmpty(): got (%v, %v), wanted (%v, <terminal iterator>)", seg, gotSeg, gotGap, wantSeg)
		}
	}
}

func TestAddSequentialNonAdjacent(t *testing.T) {
	var s Set
	var nrInsertions int
	for i := 0; i < testSize; i++ {
		// The range here differs from TestAddSequentialAdjacent so that
		// consecutive segments are not adjacent.
		s.InsertWithoutMergingRange(Range{2 * i, 2*i + 1}, 2*i+valueOffset)
		nrInsertions++
		if err := s.segmentTestCheck(nrInsertions, validate); err != nil {
			t.Errorf("Iteration %d: %v", i, err)
			break
		}
	}
	if got, want := s.countSegments(), nrInsertions; got != want {
		t.Errorf("Wrong final number of segments: got %d, wanted %d", got, want)
	}
	if t.Failed() {
		t.Logf("Set contents:\n%v", &s)
	}

	for seg := s.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		gotSeg, gotGap := seg.PrevNonEmpty()
		if wantGap := seg.PrevGap(); gotSeg.Ok() || gotGap != wantGap {
			t.Errorf("%v.PrevNonEmpty(): got (%v, %v), wanted (<terminal iterator>, %v)", seg, gotSeg, gotGap, wantGap)
		}
		gotSeg, gotGap = seg.NextNonEmpty()
		if wantGap := seg.NextGap(); gotSeg.Ok() || gotGap != wantGap {
			t.Errorf("%v.NextNonEmpty(): got (%v, %v), wanted (<terminal iterator>, %v)", seg, gotSeg, gotGap, wantGap)
		}
	}
}

func TestMerge(t *testing.T) {
	tests := []struct {
		name      string
		initial   []Range
		split     bool
		splitAddr int
		final     []Range
	}{
		{
			name:    "InsertRange merges after existing segment",
			initial: []Range{{1000, 1100}, {1100, 1200}},
			final:   []Range{{1000, 1200}},
		},
		{
			name:    "InsertRange merges before existing segment",
			initial: []Range{{1100, 1200}, {1000, 1100}},
			final:   []Range{{1000, 1200}},
		},
		{
			name:    "InsertRange merges between existing segments",
			initial: []Range{{1000, 1100}, {1200, 1300}, {1100, 1200}},
			final:   []Range{{1000, 1300}},
		},
	}
Tests:
	for _, test := range tests {
		var s Set
		for _, r := range test.initial {
			s.InsertRange(r, 0)
		}
		var i int
		for seg := s.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			if i > len(test.final) {
				t.Errorf("%s: Incorrect number of segments: got %d, wanted %d; set contents:\n%v", test.name, s.countSegments(), len(test.final), &s)
				continue Tests
			}
			if got, want := seg.Range(), test.final[i]; got != want {
				t.Errorf("%s: Segment %d mismatch: got %v, wanted %v; set contents:\n%v", test.name, i, got, want, &s)
				continue Tests
			}
			i++
		}
		if i < len(test.final) {
			t.Errorf("%s: Incorrect number of segments: got %d, wanted %d; set contents:\n%v", test.name, i, len(test.final), &s)
		}
	}
}

func TestIsolate(t *testing.T) {
	tests := []struct {
		name    string
		initial Range
		bounds  Range
		final   []Range
	}{
		{
			name:    "Isolate does not split a segment that falls inside bounds",
			initial: Range{100, 200},
			bounds:  Range{100, 200},
			final:   []Range{{100, 200}},
		},
		{
			name:    "Isolate splits at beginning of segment",
			initial: Range{50, 200},
			bounds:  Range{100, 200},
			final:   []Range{{50, 100}, {100, 200}},
		},
		{
			name:    "Isolate splits at end of segment",
			initial: Range{100, 250},
			bounds:  Range{100, 200},
			final:   []Range{{100, 200}, {200, 250}},
		},
		{
			name:    "Isolate splits at beginning and end of segment",
			initial: Range{50, 250},
			bounds:  Range{100, 200},
			final:   []Range{{50, 100}, {100, 200}, {200, 250}},
		},
	}
Tests:
	for _, test := range tests {
		var s Set
		seg := s.Insert(s.FirstGap(), test.initial, 0)
		seg = s.Isolate(seg, test.bounds)
		if !test.bounds.IsSupersetOf(seg.Range()) {
			t.Errorf("%s: Isolated segment %v lies outside bounds %v; set contents:\n%v", test.name, seg.Range(), test.bounds, &s)
		}
		var i int
		for seg := s.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			if i > len(test.final) {
				t.Errorf("%s: Incorrect number of segments: got %d, wanted %d; set contents:\n%v", test.name, s.countSegments(), len(test.final), &s)
				continue Tests
			}
			if got, want := seg.Range(), test.final[i]; got != want {
				t.Errorf("%s: Segment %d mismatch: got %v, wanted %v; set contents:\n%v", test.name, i, got, want, &s)
				continue Tests
			}
			i++
		}
		if i < len(test.final) {
			t.Errorf("%s: Incorrect number of segments: got %d, wanted %d; set contents:\n%v", test.name, i, len(test.final), &s)
		}
	}
}

func TestMutateRange(t *testing.T) {
	tests := []struct {
		name      string
		initial   []FlatSegment
		increment Range
		final     []FlatSegment
	}{
		{
			name:      "MutateRange no-op in empty set",
			increment: Range{100, 200},
		},
		{
			name: "MutateRange modifies existing segment",
			initial: []FlatSegment{
				{100, 200, 0},
			},
			increment: Range{100, 200},
			final: []FlatSegment{
				{100, 200, 1},
			},
		},
		{
			name: "MutateRange splits segments",
			initial: []FlatSegment{
				{50, 150, 0},
				{150, 250, 2},
			},
			increment: Range{100, 200},
			final: []FlatSegment{
				{50, 100, 0},
				{100, 150, 1},
				{150, 200, 3},
				{200, 250, 2},
			},
		},
		{
			name: "MutateRange merges compatible segments",
			initial: []FlatSegment{
				{0, 100, 1},
				{100, 200, 0},
				{200, 300, 1},
			},
			increment: Range{100, 200},
			final: []FlatSegment{
				{0, 300, 1},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var s Set
			if err := s.ImportSlice(test.initial); err != nil {
				t.Fatalf("Failed to import initial set: %v", err)
			}
			s.MutateRange(test.increment, func(seg Iterator) bool {
				(*seg.ValuePtr())++
				return true
			})
			if got := s.ExportSlice(); !slices.Equal(got, test.final) {
				t.Errorf("Set mismatch after mutation: got %v, wanted %v", got, test.final)
			}
		})
	}
}

func assertPanics(t *testing.T, name string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("%s: did not panic", name)
		}
	}()
	f()
}

func TestUntrackedSetZeroOverheadAndIsInvalidatedPanics(t *testing.T) {
	wordSize := reflect.TypeOf(uintptr(0)).Size()
	if got, want := reflect.TypeOf(Iterator{}).Size(), 2*wordSize; got != want {
		t.Errorf("Iterator size when trackIteratorInvalidation=0: got %d, want %d", got, want)
	}
	if got, want := reflect.TypeOf(GapIterator{}).Size(), 2*wordSize; got != want {
		t.Errorf("GapIterator size when trackIteratorInvalidation=0: got %d, want %d", got, want)
	}
	if got, want := reflect.TypeOf(Set{}).Size(), reflect.TypeOf(node{}).Size(); got != want {
		t.Errorf("Set size when trackIteratorInvalidation=0: got %d, want %d", got, want)
	}

	var s Set
	s.InsertRange(Range{10, 20}, 1)
	seg := s.FirstSegment()
	gap := s.FirstGap()

	assertPanics(t, "terminal Iterator.IsInvalidated()", func() {
		(Iterator{}).IsInvalidated()
	})
	assertPanics(t, "non-terminal Iterator.IsInvalidated()", func() {
		seg.IsInvalidated()
	})
	assertPanics(t, "terminal GapIterator.IsInvalidated()", func() {
		(GapIterator{}).IsInvalidated()
	})
	assertPanics(t, "non-terminal GapIterator.IsInvalidated()", func() {
		gap.IsInvalidated()
	})
}

func TestIteratorInvalidationTerminalIterators(t *testing.T) {
	var s invalidationWithGapsSet

	if (invalidationWithGapsIterator{}).Ok() {
		t.Errorf("zero Iterator.Ok() = true, want false")
	}
	if (invalidationWithGapsIterator{}).IsInvalidated() {
		t.Errorf("zero Iterator.IsInvalidated() = true, want false")
	}
	if (invalidationWithGapsGapIterator{}).Ok() {
		t.Errorf("zero GapIterator.Ok() = true, want false")
	}
	if (invalidationWithGapsGapIterator{}).IsInvalidated() {
		t.Errorf("zero GapIterator.IsInvalidated() = true, want false")
	}

	firstEmpty := s.FirstSegment()
	lastEmpty := s.LastSegment()
	if firstEmpty != (invalidationWithGapsIterator{}) || lastEmpty != (invalidationWithGapsIterator{}) {
		t.Errorf("empty FirstSegment/LastSegment not equal to zero Iterator: %v, %v", firstEmpty, lastEmpty)
	}

	s.InsertRange(Range{10, 20}, 1)
	if firstEmpty.Ok() || firstEmpty.IsInvalidated() {
		t.Errorf("terminal Iterator after mutation: Ok()=%v, IsInvalidated()=%v, want false, false", firstEmpty.Ok(), firstEmpty.IsInvalidated())
	}

	seg := s.FirstSegment()
	if prev := seg.PrevSegment(); prev != (invalidationWithGapsIterator{}) || prev.Ok() || prev.IsInvalidated() {
		t.Errorf("PrevSegment terminal mismatch: %v", prev)
	}
	if next := seg.NextSegment(); next != (invalidationWithGapsIterator{}) || next.Ok() || next.IsInvalidated() {
		t.Errorf("NextSegment terminal mismatch: %v", next)
	}
	if prevGap := s.FirstGap().PrevGap(); prevGap != (invalidationWithGapsGapIterator{}) || prevGap.Ok() || prevGap.IsInvalidated() {
		t.Errorf("PrevGap terminal mismatch: %v", prevGap)
	}
	if nextGap := s.LastGap().NextGap(); nextGap != (invalidationWithGapsGapIterator{}) || nextGap.Ok() || nextGap.IsInvalidated() {
		t.Errorf("NextGap terminal mismatch: %v", nextGap)
	}
	if gotGap := s.FindGap(15); gotGap != (invalidationWithGapsGapIterator{}) || gotGap.Ok() || gotGap.IsInvalidated() {
		t.Errorf("FindGap on segment mismatch: %v", gotGap)
	}
}

func TestIteratorInvalidationNonInvalidatingOperations(t *testing.T) {
	var s invalidationSet
	s.InsertWithoutMergingRange(Range{10, 20}, 1)
	s.InsertWithoutMergingRange(Range{30, 40}, 2)

	seg1 := s.FirstSegment()
	seg2 := seg1.NextSegment()
	gap := seg1.NextGap()

	// Iterator mutations do not invalidate iterators.
	seg1.SetValue(10)
	*seg1.ValuePtr() = 11
	seg1.SetStart(5)
	seg1.SetStartUnchecked(8)
	seg1.SetEnd(22)
	seg1.SetEndUnchecked(20)
	seg1.SetRange(Range{10, 20})
	seg1.SetRangeUnchecked(Range{10, 20})

	// Failed TryInsert* and failed Merge* do not invalidate iterators.
	if got := s.TryInsertRange(Range{15, 25}, 11); got.Ok() {
		t.Fatalf("TryInsertRange unexpectedly succeeded: %v", got)
	}
	if got := s.TryInsertWithoutMergingRange(Range{15, 25}, 11); got.Ok() {
		t.Fatalf("TryInsertWithoutMergingRange unexpectedly succeeded: %v", got)
	}
	// Adjacent segments with incompatible values fail Merge.
	seg1.SetEnd(30)
	if got := s.Merge(seg1, seg2); got.Ok() {
		t.Fatalf("Merge unexpectedly succeeded: %v", got)
	}
	if got := s.MergeUnchecked(seg1, seg2); got.Ok() {
		t.Fatalf("MergeUnchecked unexpectedly succeeded: %v", got)
	}

	if seg1.IsInvalidated() || seg2.IsInvalidated() || gap.IsInvalidated() {
		t.Fatalf("iterators unexpectedly invalidated: seg1=%v, seg2=%v, gap=%v", seg1.IsInvalidated(), seg2.IsInvalidated(), gap.IsInvalidated())
	}
	if got := seg2.Range(); got != (Range{30, 40}) {
		t.Errorf("seg2.Range() = %v, want [30, 40)", got)
	}
	if !gap.IsEmpty() {
		t.Errorf("gap.IsEmpty() = false, want true")
	}
}

func TestIteratorInvalidationMutatingMethods(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, s *invalidationWithGapsSet)
	}{
		{
			name: "Insert unmerged",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.Insert(s.FindGap(50), Range{50, 60}, 99)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{50, 60}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "Insert merged with prev",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.Insert(s.FindGap(20), Range{20, 25}, 1)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{10, 25}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "Insert merged with next",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.Insert(s.FindGap(25), Range{25, 30}, 2)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{25, 40}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "Insert merged with both prev and next",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.FindSegment(30).SetValue(1)
				seg := s.Insert(s.FindGap(20), Range{20, 30}, 1)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{10, 40}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "InsertWithoutMerging",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.InsertWithoutMerging(s.FindGap(20), Range{20, 25}, 1)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{20, 25}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "InsertWithoutMergingUnchecked",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.InsertWithoutMergingUnchecked(s.FindGap(20), Range{20, 25}, 1)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{20, 25}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "InsertRange",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.InsertRange(Range{50, 60}, 3)
				if !seg.Ok() || seg.IsInvalidated() {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "InsertWithoutMergingRange",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.InsertWithoutMergingRange(Range{20, 30}, 1)
				if !seg.Ok() || seg.IsInvalidated() {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "TryInsertRange success",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.TryInsertRange(Range{50, 60}, 3)
				if !seg.Ok() || seg.IsInvalidated() {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "TryInsertWithoutMergingRange success",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.TryInsertWithoutMergingRange(Range{50, 60}, 3)
				if !seg.Ok() || seg.IsInvalidated() {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "Remove",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				gap := s.Remove(s.FirstSegment())
				if !gap.Ok() || gap.IsInvalidated() || gap.End() != 30 {
					t.Fatalf("returned gap invalid: %v", gap)
				}
			},
		},
		{
			name: "RemoveAll",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.RemoveAll()
			},
		},
		{
			name: "RemoveRange with segments",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				gap := s.RemoveRange(Range{15, 35})
				if !gap.Ok() || gap.IsInvalidated() || gap.Range() != (Range{15, 35}) {
					t.Fatalf("returned gap invalid: %v", gap)
				}
			},
		},
		{
			name: "RemoveRange empty gap no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				gap := s.RemoveRange(Range{22, 28})
				if !gap.Ok() || gap.IsInvalidated() || gap.Range() != (Range{20, 30}) {
					t.Fatalf("returned gap invalid: %v", gap)
				}
			},
		},
		{
			name: "RemoveFullRange",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				gap := s.RemoveFullRange(Range{12, 18})
				if !gap.Ok() || gap.IsInvalidated() || gap.Range() != (Range{12, 18}) {
					t.Fatalf("returned gap invalid: %v", gap)
				}
			},
		},
		{
			name: "MoveFrom",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				var s2 invalidationWithGapsSet
				s2.InsertRange(Range{100, 200}, 9)
				s2Seg := s2.FirstSegment()
				s2Gap := s2.FirstGap()
				s.MoveFrom(&s2)
				if !s2Seg.IsInvalidated() || !s2Gap.IsInvalidated() {
					t.Fatalf("source set iterators not invalidated by MoveFrom")
				}
			},
		},
		{
			name: "Merge success",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				first := s.FirstSegment()
				second := first.NextSegment()
				first.SetEnd(30)
				second.SetValue(1)
				m := s.Merge(first, second)
				if !m.Ok() || m.IsInvalidated() || m.Range() != (Range{10, 40}) {
					t.Fatalf("returned merged seg invalid: %v", m)
				}
			},
		},
		{
			name: "MergePrev merge",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				first := s.FirstSegment()
				second := first.NextSegment()
				first.SetEnd(30)
				second.SetValue(1)
				m := s.MergePrev(second)
				if !m.Ok() || m.IsInvalidated() || m.Range() != (Range{10, 40}) {
					t.Fatalf("returned seg invalid: %v", m)
				}
			},
		},
		{
			name: "MergePrev no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				m := s.MergePrev(s.FirstSegment())
				if !m.Ok() || m.IsInvalidated() || m.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", m)
				}
			},
		},
		{
			name: "MergeNext no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				m := s.MergeNext(s.FirstSegment())
				if !m.Ok() || m.IsInvalidated() || m.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", m)
				}
			},
		},
		{
			name: "Unisolate no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				m := s.Unisolate(s.FirstSegment())
				if !m.Ok() || m.IsInvalidated() || m.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", m)
				}
			},
		},
		{
			name: "MergeAll no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.MergeAll()
			},
		},
		{
			name: "MergeInsideRange no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.MergeInsideRange(Range{0, 100})
			},
		},
		{
			name: "MergeOutsideRange no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.MergeOutsideRange(Range{10, 40})
			},
		},
		{
			name: "Split",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s1, s2 := s.Split(s.FirstSegment(), 15)
				if !s1.Ok() || s1.IsInvalidated() || s1.Range() != (Range{10, 15}) {
					t.Fatalf("s1 invalid: %v", s1)
				}
				if !s2.Ok() || s2.IsInvalidated() || s2.Range() != (Range{15, 20}) {
					t.Fatalf("s2 invalid: %v", s2)
				}
			},
		},
		{
			name: "SplitBefore no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.SplitBefore(s.FirstSegment(), 10)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "SplitAfter no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.SplitAfter(s.FirstSegment(), 20)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "Isolate no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.Isolate(s.FirstSegment(), Range{0, 100})
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "LowerBoundSegmentSplitBefore in gap",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.LowerBoundSegmentSplitBefore(25)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{30, 40}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "UpperBoundSegmentSplitAfter in gap",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				seg := s.UpperBoundSegmentSplitAfter(25)
				if !seg.Ok() || seg.IsInvalidated() || seg.Range() != (Range{10, 20}) {
					t.Fatalf("returned seg invalid: %v", seg)
				}
			},
		},
		{
			name: "MutateRange empty no-op",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.MutateRange(Range{22, 28}, func(seg invalidationWithGapsIterator) bool {
					return true
				})
			},
		},
		{
			name: "MutateFullRange",
			mutate: func(t *testing.T, s *invalidationWithGapsSet) {
				s.MutateFullRange(Range{12, 18}, func(seg invalidationWithGapsIterator) bool {
					seg.SetValue(42)
					return true
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var s invalidationWithGapsSet
			s.InsertWithoutMergingRange(Range{10, 20}, 1)
			s.InsertWithoutMergingRange(Range{30, 40}, 2)

			oldSeg := s.FirstSegment()
			oldGap := s.FirstGap()
			if oldSeg.IsInvalidated() || oldGap.IsInvalidated() {
				t.Fatalf("iterators invalidated before mutation")
			}

			tc.mutate(t, &s)

			// Ok() must not panic on invalidated iterators.
			if !oldSeg.Ok() {
				t.Errorf("oldSeg.Ok() = false, want true")
			}
			if !oldGap.Ok() {
				t.Errorf("oldGap.Ok() = false, want true")
			}
			if !oldSeg.IsInvalidated() {
				t.Errorf("oldSeg.IsInvalidated() = false, want true")
			}
			if !oldGap.IsInvalidated() {
				t.Errorf("oldGap.IsInvalidated() = false, want true")
			}
		})
	}
}

func TestIteratorInvalidationUsePanics(t *testing.T) {
	var s invalidationWithGapsSet
	s.InsertWithoutMergingRange(Range{10, 20}, 1)
	s.InsertWithoutMergingRange(Range{30, 40}, 2)

	seg := s.FirstSegment()
	seg2 := seg.NextSegment()
	gap := seg.NextGap()

	// Invalidate seg, seg2, and gap.
	s.InsertWithoutMergingRange(Range{50, 60}, 3)

	if !seg.Ok() || !seg.IsInvalidated() {
		t.Fatalf("expected seg to be non-terminal and invalidated")
	}
	if !gap.Ok() || !gap.IsInvalidated() {
		t.Fatalf("expected gap to be non-terminal and invalidated")
	}

	// Using invalidated Iterator methods must panic.
	assertPanics(t, "Iterator.Range", func() { seg.Range() })
	assertPanics(t, "Iterator.Start", func() { seg.Start() })
	assertPanics(t, "Iterator.End", func() { seg.End() })
	assertPanics(t, "Iterator.SetRange", func() { seg.SetRange(Range{10, 20}) })
	assertPanics(t, "Iterator.SetRangeUnchecked", func() { seg.SetRangeUnchecked(Range{10, 20}) })
	assertPanics(t, "Iterator.SetStart", func() { seg.SetStart(10) })
	assertPanics(t, "Iterator.SetStartUnchecked", func() { seg.SetStartUnchecked(10) })
	assertPanics(t, "Iterator.SetEnd", func() { seg.SetEnd(20) })
	assertPanics(t, "Iterator.SetEndUnchecked", func() { seg.SetEndUnchecked(20) })
	assertPanics(t, "Iterator.Value", func() { seg.Value() })
	assertPanics(t, "Iterator.ValuePtr", func() { seg.ValuePtr() })
	assertPanics(t, "Iterator.SetValue", func() { seg.SetValue(5) })
	assertPanics(t, "Iterator.PrevSegment", func() { seg.PrevSegment() })
	assertPanics(t, "Iterator.NextSegment", func() { seg.NextSegment() })
	assertPanics(t, "Iterator.PrevGap", func() { seg.PrevGap() })
	assertPanics(t, "Iterator.NextGap", func() { seg.NextGap() })
	assertPanics(t, "Iterator.PrevNonEmpty", func() { seg.PrevNonEmpty() })
	assertPanics(t, "Iterator.NextNonEmpty", func() { seg.NextNonEmpty() })

	// Using invalidated GapIterator methods must panic.
	assertPanics(t, "GapIterator.Range", func() { gap.Range() })
	assertPanics(t, "GapIterator.Start", func() { gap.Start() })
	assertPanics(t, "GapIterator.End", func() { gap.End() })
	assertPanics(t, "GapIterator.IsEmpty", func() { gap.IsEmpty() })
	assertPanics(t, "GapIterator.PrevSegment", func() { gap.PrevSegment() })
	assertPanics(t, "GapIterator.NextSegment", func() { gap.NextSegment() })
	assertPanics(t, "GapIterator.PrevGap", func() { gap.PrevGap() })
	assertPanics(t, "GapIterator.NextGap", func() { gap.NextGap() })
	assertPanics(t, "GapIterator.NextLargeEnoughGap", func() { gap.NextLargeEnoughGap(1) })
	assertPanics(t, "GapIterator.PrevLargeEnoughGap", func() { gap.PrevLargeEnoughGap(1) })

	// Passing invalidated iterators to Set methods must panic.
	validFirst := s.FirstSegment()
	validSecond := validFirst.NextSegment()
	assertPanics(t, "Set.Insert", func() { s.Insert(gap, Range{22, 28}, 9) })
	assertPanics(t, "Set.InsertWithoutMerging", func() { s.InsertWithoutMerging(gap, Range{22, 28}, 9) })
	assertPanics(t, "Set.InsertWithoutMergingUnchecked", func() { s.InsertWithoutMergingUnchecked(gap, Range{22, 28}, 9) })
	assertPanics(t, "Set.Remove", func() { s.Remove(seg) })
	assertPanics(t, "Set.Merge first", func() { s.Merge(seg, validSecond) })
	assertPanics(t, "Set.Merge second", func() { s.Merge(validFirst, seg2) })
	assertPanics(t, "Set.MergeUnchecked first", func() { s.MergeUnchecked(seg, validSecond) })
	assertPanics(t, "Set.MergeUnchecked second", func() { s.MergeUnchecked(validFirst, seg2) })
	assertPanics(t, "Set.MergePrev", func() { s.MergePrev(seg) })
	assertPanics(t, "Set.MergeNext", func() { s.MergeNext(seg) })
	assertPanics(t, "Set.Unisolate", func() { s.Unisolate(seg) })
	assertPanics(t, "Set.Split", func() { s.Split(seg, 15) })
	assertPanics(t, "Set.SplitUnchecked", func() { s.SplitUnchecked(seg, 15) })
	assertPanics(t, "Set.SplitBefore", func() { s.SplitBefore(seg, 15) })
	assertPanics(t, "Set.SplitAfter", func() { s.SplitAfter(seg, 15) })
	assertPanics(t, "Set.Isolate", func() { s.Isolate(seg, Range{12, 18}) })

	// Passing iterators from a different Set must panic.
	var otherSet invalidationWithGapsSet
	otherSet.InsertWithoutMergingRange(Range{10, 20}, 1)
	otherSeg := otherSet.FirstSegment()
	otherGap := otherSet.FindGap(25)
	assertPanics(t, "Set.Remove foreign iterator", func() { s.Remove(otherSeg) })
	assertPanics(t, "Set.Insert foreign gap iterator", func() { s.Insert(otherGap, Range{25, 26}, 1) })

	// MoveFrom must not allow stale iterators to become valid again via counter overwrite.
	var dst, src invalidationWithGapsSet
	dst.InsertRange(Range{1, 2}, 1)
	staleDstSeg := dst.FirstSegment() // count snapshot = 1
	src.InsertRange(Range{5, 6}, 5)   // src count = 1
	// Mutate dst once more so dst count = 2 before MoveFrom, or keep dst count = 0 and src count = 0:
	var freshDst invalidationWithGapsSet
	freshGap := freshDst.FirstGap()      // count snapshot = 0
	var emptySrc invalidationWithGapsSet // count = 0
	freshDst.MoveFrom(&emptySrc)
	if !freshGap.IsInvalidated() {
		t.Errorf("freshGap not invalidated after MoveFrom from fresh source set")
	}
	dst.MoveFrom(&src)
	if !staleDstSeg.IsInvalidated() {
		t.Errorf("staleDstSeg not invalidated after MoveFrom")
	}

	// ImportSlice invalidates existing gap iterators on an empty set.
	var importSet invalidationWithGapsSet
	emptyGap := importSet.FirstGap()
	if err := importSet.ImportSlice(nil); err != nil {
		t.Fatalf("ImportSlice(nil) failed: %v", err)
	}
	if !emptyGap.IsInvalidated() {
		t.Errorf("emptyGap not invalidated by ImportSlice(nil)")
	}

	// Mutating inside VisitRange / MutateRange callback panics.
	assertPanics(t, "VisitRange callback invalidates", func() {
		s.VisitRange(Range{0, 100}, func(it invalidationWithGapsIterator) bool {
			s.InsertRange(Range{70, 80}, 4)
			return true
		})
	})
	assertPanics(t, "MutateRange callback invalidates", func() {
		s.MutateRange(Range{0, 100}, func(it invalidationWithGapsIterator) bool {
			s.InsertRange(Range{90, 100}, 5)
			return true
		})
	})
}

func TestIteratorInvalidationStressRandom(t *testing.T) {
	var s invalidationWithGapsSet
	order := randIntervalPermutation(testSize)
	for _, j := range order {
		s.InsertRange(Range{j, j + rand.Intn(intervalLength-1) + 1}, j+valueOffset)
	}

	// Exercise Forward/Backward segment and gap iteration, including LargeEnoughGap.
	minSize := 5
	for gap := s.FirstLargeEnoughGap(minSize); gap.Ok(); gap = gap.NextLargeEnoughGap(minSize) {
		if gap.IsInvalidated() {
			t.Fatalf("gap unexpectedly invalidated during NextLargeEnoughGap")
		}
		if gap.Range().Length() < minSize {
			t.Fatalf("gap %v smaller than %d", gap.Range(), minSize)
		}
	}
	for gap := s.LastLargeEnoughGap(minSize); gap.Ok(); gap = gap.PrevLargeEnoughGap(minSize) {
		if gap.IsInvalidated() {
			t.Fatalf("gap unexpectedly invalidated during PrevLargeEnoughGap")
		}
		if gap.Range().Length() < minSize {
			t.Fatalf("gap %v smaller than %d", gap.Range(), minSize)
		}
	}

	shuffle(order)
	for _, j := range order[:testSize/2] {
		seg := s.FindSegment(j)
		if seg.Ok() {
			s.Remove(seg)
		}
	}
	s.MutateRange(Range{0, intervalLength * testSize / 2}, func(seg invalidationWithGapsIterator) bool {
		seg.SetValue(0)
		return true
	})
	s.MergeAll()
	s.RemoveRange(Range{intervalLength * testSize / 4, intervalLength * testSize / 2})
	for seg := s.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
		if seg.IsInvalidated() {
			t.Fatalf("seg unexpectedly invalidated")
		}
	}
}

func benchmarkAddSequential(b *testing.B, size int) {
	for n := 0; n < b.N; n++ {
		var s Set
		for i := 0; i < size; i++ {
			s.InsertWithoutMergingRange(Range{i, i + 1}, i)
		}
	}
}

func benchmarkAddRandom(b *testing.B, size int) {
	order := rand.Perm(size)

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var s Set
		for _, i := range order {
			s.InsertWithoutMergingRange(Range{i, i + 1}, i)
		}
	}
}

func benchmarkFindSequential(b *testing.B, size int) {
	var s Set
	for i := 0; i < size; i++ {
		s.InsertWithoutMergingRange(Range{i, i + 1}, i)
	}

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for i := 0; i < size; i++ {
			if seg := s.FindSegment(i); !seg.Ok() {
				b.Fatalf("Failed to find segment %d", i)
			}
		}
	}
}

func benchmarkFindRandom(b *testing.B, size int) {
	var s Set
	for i := 0; i < size; i++ {
		s.InsertWithoutMergingRange(Range{i, i + 1}, i)
	}
	order := rand.Perm(size)

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		for _, i := range order {
			if si := s.FindSegment(i); !si.Ok() {
				b.Fatalf("Failed to find segment %d", i)
			}
		}
	}
}

func benchmarkIteration(b *testing.B, size int) {
	var s Set
	for i := 0; i < size; i++ {
		s.InsertWithoutMergingRange(Range{i, i + 1}, i)
	}

	b.ResetTimer()
	var count uint64
	for n := 0; n < b.N; n++ {
		for seg := s.FirstSegment(); seg.Ok(); seg = seg.NextSegment() {
			count++
		}
	}
	if got, want := count, uint64(size)*uint64(b.N); got != want {
		b.Fatalf("Iterated wrong number of segments: got %d, wanted %d", got, want)
	}
}

func benchmarkAddFindRemoveSequential(b *testing.B, size int) {
	for n := 0; n < b.N; n++ {
		var s Set
		for i := 0; i < size; i++ {
			s.InsertWithoutMergingRange(Range{i, i + 1}, i)
		}
		for i := 0; i < size; i++ {
			seg := s.FindSegment(i)
			if !seg.Ok() {
				b.Fatalf("Failed to find segment %d", i)
			}
			s.Remove(seg)
		}
		if !s.IsEmpty() {
			b.Fatalf("Set not empty after all removals:\n%v", &s)
		}
	}
}

func benchmarkAddFindRemoveRandom(b *testing.B, size int) {
	order := rand.Perm(size)

	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		var s Set
		for _, i := range order {
			s.InsertWithoutMergingRange(Range{i, i + 1}, i)
		}
		for _, i := range order {
			seg := s.FindSegment(i)
			if !seg.Ok() {
				b.Fatalf("Failed to find segment %d", i)
			}
			s.Remove(seg)
		}
		if !s.IsEmpty() {
			b.Fatalf("Set not empty after all removals:\n%v", &s)
		}
	}
}

// Although we don't generally expect our segment sets to get this big, they're
// useful for emulating the effect of cache pressure.
var testSizes = []struct {
	desc string
	size int
}{
	{"64", 1 << 6},
	{"256", 1 << 8},
	{"1K", 1 << 10},
	{"4K", 1 << 12},
	{"16K", 1 << 14},
	{"64K", 1 << 16},
}

func BenchmarkAddSequential(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkAddSequential(b, test.size)
		})
	}
}

func BenchmarkAddRandom(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkAddRandom(b, test.size)
		})
	}
}

func BenchmarkFindSequential(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkFindSequential(b, test.size)
		})
	}
}

func BenchmarkFindRandom(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkFindRandom(b, test.size)
		})
	}
}

func BenchmarkIteration(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkIteration(b, test.size)
		})
	}
}

func BenchmarkAddFindRemoveSequential(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkAddFindRemoveSequential(b, test.size)
		})
	}
}

func BenchmarkAddFindRemoveRandom(b *testing.B) {
	for _, test := range testSizes {
		b.Run(test.desc, func(b *testing.B) {
			benchmarkAddFindRemoveRandom(b, test.size)
		})
	}
}
