package rewrite

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"tfdatasetimport/observe"
)

// altSource stands in for a second implementation: it builds the HCL itself rather than asking the
// meta API, and fills in nothing but Resource and ImportID.
type altSource struct{}

func (altSource) GetDatasetTerraform(_ context.Context, id string) (*observe.TerraformDefinition, error) {
	hcl := fmt.Sprintf(`resource "observe_dataset" "ignored" {
  name      = "Reconstructed/%s"
  workspace = "o:::workspace:99000001"
  inputs = {
    "peer" = "o:::dataset:99001000"
  }
  stage {
    pipeline = <<-PIPE
      filter true
    PIPE
  }
}`, id)
	return &observe.TerraformDefinition{ImportID: id, Resource: hcl}, nil
}

// TestAlternateSourceDrivesTheWholePipeline guards the seam: nothing downstream of observe.Source may
// depend on how a definition was produced.
//
// altSource is the minimum a second implementation can supply — it builds the HCL itself instead of
// asking the meta API, and fills in nothing but Resource and ImportID.
func TestAlternateSourceDrivesTheWholePipeline(t *testing.T) {
	h := newHarness(t)

	var src observe.Source = altSource{}
	def, err := src.GetDatasetTerraform(context.Background(), "12345")
	if err != nil {
		t.Fatal(err)
	}

	// ImportName was left empty, so the tool has to derive a resource name.
	if got := DatasetName(def); got != "Reconstructed/12345" {
		t.Fatalf("dataset name = %q", got)
	}
	if got := sanitizeIdentifier(DatasetName(def)); got != "_12345" {
		t.Fatalf("derived name = %q", got)
	}

	res, err := (&Rewriter{Refs: h.refs, FreshnessDefault: "5m"}).Rewrite(def, "reconstructed", nil)
	if err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	got := string(res.HCL)
	for _, want := range []string{
		`workspace = var.workspace.oid`,
		`name = format(var.name_format, "Reconstructed/12345")`,
		`"peer" = observe_dataset.existing_peer.oid`,
	} {
		if !strings.Contains(squashSpaces(got), squashSpaces(want)) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
