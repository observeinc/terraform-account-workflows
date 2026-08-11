// Package output writes the artifacts a run produces: the manifest, the import and rollback
// scripts, and the human-readable report.
package output

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"

	"tfdatasetimport/rewrite"
	"tfdatasetimport/tf"
)

// Entry is one dataset's manifest record. The manifest decouples generation from script emission,
// so a script can be regenerated, or an import replayed, without re-fetching.
type Entry struct {
	SourceDatasetID   string `json:"source_dataset_id"`
	SourceDatasetName string `json:"source_dataset_name"`
	ResourceName      string `json:"resource_name"`
	File              string `json:"file"`
	ImportAddress     string `json:"import_address"`
	HasGrants         bool   `json:"has_grants,omitempty"`
	// AlreadyManaged is true when the dataset was already under this repo's management (state or
	// config) before this run — an update, not a create. No new import block is written for it.
	AlreadyManaged bool `json:"already_managed,omitempty"`
	// GrantsAlreadyManaged is true when an observe_resource_grants resource under this name is
	// already declared or already in state, independent of AlreadyManaged: a dataset can be an
	// update while its grants companion is new, e.g. it had no grants at creation and one was added
	// in Observe since. Only meaningful when HasGrants is true.
	GrantsAlreadyManaged bool              `json:"grants_already_managed,omitempty"`
	Freshness            string            `json:"freshness,omitempty"`
	FreshnessSynthesized bool              `json:"freshness_synthesized,omitempty"`
	CorrelationTags      []string          `json:"correlation_tags,omitempty"`
	ResolvedRefs         map[string]string `json:"resolved_refs,omitempty"`
	ReviewRefs           map[string]string `json:"review_refs,omitempty"`
	UnresolvedRefs       []string          `json:"unresolved_refs,omitempty"`
	UnknownResources     []string          `json:"unknown_resources,omitempty"`
}

// NeedsDatasetImport reports whether this entry's observe_dataset resource needs a new import block.
func (e Entry) NeedsDatasetImport() bool { return !e.AlreadyManaged }

// NeedsGrantsImport reports whether this entry's observe_resource_grants companion needs a new import
// block. False whenever the dataset carries no grants at all.
func (e Entry) NeedsGrantsImport() bool { return e.HasGrants && !e.GrantsAlreadyManaged }

// Manifest is the full run record.
type Manifest struct {
	CustomerID    string  `json:"customer_id"`
	Domain        string  `json:"domain"`
	ModuleDir     string  `json:"module_dir"`
	ModuleAddress string  `json:"module_address"`
	TFWorkspace   string  `json:"tf_workspace"`
	Entries       []Entry `json:"entries"`
	// BlastRadius is the count of datasets downstream of the imported set. Any config change
	// recomputes an imported dataset's oid, which cascades an update to all of them.
	BlastRadius int `json:"blast_radius"`
}

// NewEntry builds a manifest entry from a rewrite result.
func NewEntry(res *rewrite.Result, file, importAddress string) Entry {
	e := Entry{
		SourceDatasetID:      res.DatasetID,
		SourceDatasetName:    res.DatasetName,
		ResourceName:         res.ResourceName,
		File:                 file,
		ImportAddress:        importAddress,
		Freshness:            res.Freshness,
		FreshnessSynthesized: res.FreshnessSynthesized,
		CorrelationTags:      res.CorrelationTags,
		UnknownResources:     res.UnknownResources,
	}

	for _, ref := range res.Refs {
		switch ref.Tier {
		case tf.TierUnresolved:
			e.UnresolvedRefs = append(e.UnresolvedRefs, ref.OID)
		case tf.TierModuleData:
			if e.ReviewRefs == nil {
				e.ReviewRefs = map[string]string{}
			}
			e.ReviewRefs[ref.OID] = ref.Reference
		default:
			if e.ResolvedRefs == nil {
				e.ResolvedRefs = map[string]string{}
			}
			e.ResolvedRefs[ref.OID] = ref.Reference
		}
	}
	sort.Strings(e.UnresolvedRefs)
	return e
}

// PendingImportCount counts the import blocks WriteImportBlocks would write for this manifest right
// now — the dataset itself for every newly-created entry, plus the grants companion for any entry
// whose grants are not yet imported either. Used to preview a run without writing anything.
func PendingImportCount(m *Manifest) int {
	n := 0
	for _, e := range m.Entries {
		if e.NeedsDatasetImport() {
			n++
		}
		if e.NeedsGrantsImport() {
			n++
		}
	}
	return n
}

// Write serializes the manifest to path.
func (m *Manifest) Write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}
