package observe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Cache wraps a Source with an on-disk store, so re-running the tool after a rewrite-rule change
// costs no API calls. Entries are keyed by customer and dataset id and never expire — a stale
// entry is cleared by deleting the directory.
type Cache struct {
	Dir        string
	CustomerID string
	Inner      Source

	Hits, Misses int
}

// NewCache returns a caching Source writing under dir.
func NewCache(dir, customerID string, inner Source) *Cache {
	return &Cache{Dir: dir, CustomerID: customerID, Inner: inner}
}

func (c *Cache) path(datasetID string) string {
	return filepath.Join(c.Dir, c.CustomerID, datasetID+".json")
}

// GetDatasetTerraform returns the cached definition when present, otherwise delegates and stores
// the result. A cache write failure is not fatal: the definition is already in hand.
func (c *Cache) GetDatasetTerraform(ctx context.Context, datasetID string) (*TerraformDefinition, error) {
	path := c.path(datasetID)
	if b, err := os.ReadFile(path); err == nil {
		var def TerraformDefinition
		if err := json.Unmarshal(b, &def); err == nil && def.Resource != "" {
			c.Hits++
			return &def, nil
		}
	}

	def, err := c.Inner.GetDatasetTerraform(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	c.Misses++

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return def, nil
	}
	b, err := json.MarshalIndent(def, "", "  ")
	if err != nil {
		return def, nil
	}
	_ = os.WriteFile(path, b, 0o644)
	return def, nil
}

// DirSource serves definitions straight from a directory of captured responses, with no network.
// Tests and offline reruns use it.
type DirSource struct{ Dir string }

// GetDatasetTerraform reads <Dir>/<datasetID>.json.
func (f *DirSource) GetDatasetTerraform(_ context.Context, datasetID string) (*TerraformDefinition, error) {
	b, err := os.ReadFile(filepath.Join(f.Dir, datasetID+".json"))
	if err != nil {
		return nil, fmt.Errorf("fixture for dataset %s: %w", datasetID, err)
	}
	var def TerraformDefinition
	if err := json.Unmarshal(b, &def); err != nil {
		return nil, fmt.Errorf("fixture for dataset %s: %w", datasetID, err)
	}
	return &def, nil
}
