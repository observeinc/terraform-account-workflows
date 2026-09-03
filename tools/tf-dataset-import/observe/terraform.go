package observe

import (
	"context"
	"fmt"
)

// getTerraformQuery asks for only the fields TerraformDefinition carries. The API also returns a
// `dataSource` rendering of the same object, which this tool has no use for: the target is a resource
// in a module, not a data source.
const getTerraformQuery = `query GetTerraform($id: ObjectId!, $type: TerraformObjectType!) {
  getTerraform(id: $id, type: $type) {
    resource
    importId
    importName
  }
}`

// GetDatasetTerraform asks the meta API to generate terraform for one dataset, making Client the
// default Source.
//
// Server-side this round-trips through the real terraform provider — a refresh followed by a
// `terraform show` — so the result reflects the live definition rather than a reconstruction of it.
// That fidelity is the reason it is the default, and it costs about half a second per dataset.
func (c *Client) GetDatasetTerraform(ctx context.Context, datasetID string) (*TerraformDefinition, error) {
	var out struct {
		GetTerraform TerraformDefinition `json:"getTerraform"`
	}
	vars := map[string]any{"id": datasetID, "type": "Dataset"}
	if err := c.Query(ctx, getTerraformQuery, vars, &out); err != nil {
		return nil, fmt.Errorf("getTerraform(%s): %w", datasetID, err)
	}
	if out.GetTerraform.Resource == "" {
		return nil, fmt.Errorf("getTerraform(%s): empty resource body", datasetID)
	}
	return &out.GetTerraform, nil
}
