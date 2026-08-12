package tf

import (
	"fmt"
	"sort"
)

// Edit replaces the byte range [Start, End) of a file's original contents with Replacement. Start
// and End are byte offsets into the file's ORIGINAL, unmodified contents — never into a
// partially-edited buffer, since ApplySplices computes every edit's position against that same
// original and applies all of them in a single pass. An edit with Start == End is a pure insert:
// nothing is removed, Replacement lands at that position.
type Edit struct {
	Start, End  int
	Replacement []byte
}

// ApplySplices applies a set of non-overlapping edits to src in one pass, sorted by position.
//
// This is how an update rewrites a resource that lives in a file declaring other resources too:
// each resource this run touches contributes one edit — a replace for a resource being updated in
// place (Start/End from its ExistingResource.Range), an insert for one being added fresh right next
// to it (Start == End at the insertion point) — and every edit's offsets are computed against, and
// applied against, the same original bytes, so a later edit is never invalidated by an earlier one
// having already shifted the buffer. This is the same technique AddFreshnessOverrides already uses
// for a single map's worth of value edits, generalized to an arbitrary set of edits over a whole
// file.
//
// Two edits whose ranges overlap are rejected rather than silently applied in whichever order
// sort.Slice happens to produce: nothing in this tool ever intends for two different results to
// claim the same span of an existing file, so an overlap means a bug upstream (e.g. two results
// both claiming the same resource name), not an ordering decision to make here.
func ApplySplices(src []byte, edits []Edit) ([]byte, error) {
	if len(edits) == 0 {
		return src, nil
	}

	sorted := make([]Edit, len(edits))
	copy(sorted, edits)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Start < sorted[j].Start })

	out := make([]byte, 0, len(src))
	pos := 0
	for _, e := range sorted {
		if e.Start < 0 || e.End < e.Start || e.End > len(src) {
			return nil, fmt.Errorf("edit range [%d,%d) out of bounds for %d-byte source", e.Start, e.End, len(src))
		}
		if e.Start < pos {
			return nil, fmt.Errorf("overlapping edit at byte %d (a previous edit already extends to %d)", e.Start, pos)
		}
		out = append(out, src[pos:e.Start]...)
		out = append(out, e.Replacement...)
		pos = e.End
	}
	out = append(out, src[pos:]...)
	return out, nil
}
