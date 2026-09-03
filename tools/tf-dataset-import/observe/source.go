package observe

import "context"

// TerraformDefinition is one dataset's generated terraform.
//
// This is the contract between a Source and the rest of the tool, so it is deliberately small:
//
//   - Resource is the only required field: an HCL `resource "observe_dataset" "<name>" { … }` block,
//     optionally followed by `observe_correlation_tag` blocks. Attribute order does not matter and
//     hardcoded oids are expected — rewriting those is the tool's job, not a Source's.
//   - ImportID is the bare numeric dataset id `terraform import` takes.
//   - ImportName is a suggested terraform resource name. Optional: when empty the tool derives one
//     from the dataset name using the same rule the meta API's resolver applies.
//
// The JSON tags match the meta API's getTerraform payload, because that is also the on-disk cache
// format — which is what lets a captured response be replayed as a test fixture.
type TerraformDefinition struct {
	Resource   string `json:"resource"`
	ImportID   string `json:"importId"`
	ImportName string `json:"importName"`
}

// Source turns a dataset id into its generated terraform.
//
// This is the tool's one seam, and everything downstream consumes a TerraformDefinition without
// learning where it came from. A different way of producing one — reconstructing it from O2
// self-monitoring telemetry rather than asking the meta API to generate it — is a new Source and
// nothing else: Cache wraps any Source, and DirSource already replaces it wholesale in tests.
type Source interface {
	GetDatasetTerraform(ctx context.Context, datasetID string) (*TerraformDefinition, error)
}

// Every Source in this package, asserted at compile time so an implementation cannot drift from the
// interface unnoticed.
var (
	_ Source = (*Client)(nil)
	_ Source = (*Cache)(nil)
	_ Source = (*DirSource)(nil)
)
