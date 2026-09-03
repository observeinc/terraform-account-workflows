package tf

import (
	"strings"
	"testing"
)

func TestApplySplicesNoEditsReturnsSrcUnchanged(t *testing.T) {
	src := []byte("resource \"a\" \"b\" {}\n")
	got, err := ApplySplices(src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(src) {
		t.Errorf("got %q, want src unchanged", got)
	}
}

func TestApplySplicesSingleReplace(t *testing.T) {
	src := []byte("AAA BBB CCC")
	got, err := ApplySplices(src, []Edit{{Start: 4, End: 7, Replacement: []byte("xyz")}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "AAA xyz CCC"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestApplySplicesInsertAtZeroWidthRange(t *testing.T) {
	src := []byte("AAA CCC")
	// An insert is Start == End: nothing removed, Replacement lands at that position.
	got, err := ApplySplices(src, []Edit{{Start: 4, End: 4, Replacement: []byte("BBB ")}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "AAA BBB CCC"; string(got) != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestApplySplicesMultipleEditsInOneFile is the case an update actually needs: a dataset resource
// being replaced in place and its grants companion being freshly inserted right after it, in a
// single pass over one file — the scenario a hand-written file with an already-managed dataset but
// no grants block yet presents.
func TestApplySplicesMultipleEditsInOneFile(t *testing.T) {
	src := []byte(`resource "observe_dataset" "before" {
  name = "before"
}

resource "observe_dataset" "target" {
  name = "old"
}
`)
	targetStart := strings.Index(string(src), `resource "observe_dataset" "target" {`)
	if targetStart < 0 {
		t.Fatal("target block not found in fixture")
	}
	// The target block's own closing "}\n", searched from targetStart so the "before" block's own
	// closing brace earlier in the file cannot be mistaken for it.
	closeRel := strings.Index(string(src[targetStart:]), "}\n")
	if closeRel < 0 {
		t.Fatal("target block's closing brace not found in fixture")
	}
	targetEnd := targetStart + closeRel + len("}\n")
	insertAt := targetEnd

	edits := []Edit{
		// Passed out of position order deliberately, to prove sorting-before-apply is real.
		{Start: insertAt, End: insertAt, Replacement: []byte("\nresource \"observe_resource_grants\" \"target\" {\n  oid = observe_dataset.target.oid\n}\n")},
		{Start: targetStart, End: targetEnd, Replacement: []byte("resource \"observe_dataset\" \"target\" {\n  name = \"new\"\n}\n")},
	}

	got, err := ApplySplices(src, edits)
	if err != nil {
		t.Fatal(err)
	}
	gotStr := string(got)
	for _, want := range []string{
		`resource "observe_dataset" "before"`,
		`name = "before"`,
		`name = "new"`,
		`resource "observe_resource_grants" "target"`,
	} {
		if !strings.Contains(gotStr, want) {
			t.Errorf("missing %q in:\n%s", want, gotStr)
		}
	}
	if strings.Contains(gotStr, `name = "old"`) {
		t.Errorf("old target content should have been replaced, not kept:\n%s", gotStr)
	}
}

func TestApplySplicesRejectsOverlappingEdits(t *testing.T) {
	src := []byte("AAAAAAAAAA")
	_, err := ApplySplices(src, []Edit{
		{Start: 0, End: 5, Replacement: []byte("x")},
		{Start: 3, End: 8, Replacement: []byte("y")},
	})
	if err == nil {
		t.Fatal("want an error for overlapping edits")
	}
}

func TestApplySplicesRejectsOutOfBoundsEdit(t *testing.T) {
	src := []byte("AAAAA")
	_, err := ApplySplices(src, []Edit{{Start: 3, End: 10, Replacement: []byte("x")}})
	if err == nil {
		t.Fatal("want an error for an edit range past the end of src")
	}
}
