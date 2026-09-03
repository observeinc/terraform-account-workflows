package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"tfdatasetimport/rewrite"
	"tfdatasetimport/tf"
)

// resultPlacement is where one result's dataset resource (and, if it has any, its grants companion)
// actually belongs on disk.
//
// A brand-new dataset — one this run is adopting for the first time — gets a file this tool fully
// owns, named after its resource, exactly as it always has. An already-managed dataset gets updated
// in place instead: its own byte range, in whatever file the module already declares it in, is
// replaced with its freshly generated content, and its grants companion is replaced in place (if it
// already exists) or inserted right after the dataset's own block (if it does not) — never by
// writing a second file that happens to duplicate what is already there. An earlier version of this
// tool got exactly that wrong: a dataset already declared in a hand-written file grouping several
// datasets together (e.g. grouped.tf) was still written to a second file named after its own
// resource name, producing two declarations of the same resource address in the same module.
type resultPlacement struct {
	// DatasetFile is the absolute path the dataset resource itself lives (or will live) in.
	DatasetFile string
	// RelFile is DatasetFile relative to the repo root, for the manifest and report.
	RelFile string
	// New is true when DatasetFile does not exist yet and this result is written as a whole new
	// file (res.HCL, unchanged from how a first-time adoption has always been written). When false,
	// edits below are what to apply instead of a whole-file write.
	New bool
	// edits are the splices needed to bring DatasetFile (and, if the grants companion lives
	// elsewhere, that other file) up to date. Empty when New. Grouped across every result by the
	// caller, keyed by absolute file, before any of them are applied — a single file can receive
	// edits from more than one result, and they all have to be computed against, and applied to,
	// the same original bytes in one pass.
	edits []fileEdit
}

// fileEdit is one edit destined for a specific file, before grouping.
type fileEdit struct {
	file string // absolute
	edit tf.Edit
}

// planResult resolves where res belongs on disk.
//
// repo.ExistingDatasets and repo.ExistingGrants are what make an in-place update possible: they
// record exactly where a dataset resource, and independently its grants companion, are already
// declared — file and byte range both — so this never has to assume a managed resource lives in a
// file named after it.
func planResult(repo *tf.Repo, res *rewrite.Result) (resultPlacement, error) {
	key := strings.ToLower(res.ResourceName)

	existingDataset, managed := repo.ExistingDatasets[key]
	if !managed {
		abs := filepath.Join(repo.ModulePath(), res.ResourceName+".tf")
		rel, err := filepath.Rel(repo.Root, abs)
		if err != nil {
			return resultPlacement{}, err
		}
		return resultPlacement{DatasetFile: abs, RelFile: rel, New: true}, nil
	}

	rel, err := filepath.Rel(repo.Root, existingDataset.File)
	if err != nil {
		return resultPlacement{}, err
	}
	p := resultPlacement{DatasetFile: existingDataset.File, RelFile: rel}
	p.edits = append(p.edits, fileEdit{
		file: existingDataset.File,
		edit: tf.Edit{
			Start:       existingDataset.Range.Start.Byte,
			End:         existingDataset.Range.End.Byte,
			Replacement: res.DatasetHCL,
		},
	})

	if len(res.GrantsHCL) == 0 {
		return p, nil
	}
	if existingGrants, ok := repo.ExistingGrants[key]; ok {
		// Already declared somewhere (almost always the same file as the dataset, but not
		// necessarily — see tf.scanExistingResources): replace it in place, wherever it lives.
		p.edits = append(p.edits, fileEdit{
			file: existingGrants.File,
			edit: tf.Edit{
				Start:       existingGrants.Range.Start.Byte,
				End:         existingGrants.Range.End.Byte,
				Replacement: res.GrantsHCL,
			},
		})
	} else {
		// Not declared anywhere yet: insert right after the dataset's own block, in the same file,
		// rather than off in a file of its own.
		p.edits = append(p.edits, fileEdit{
			file: existingDataset.File,
			edit: tf.Edit{
				Start:       existingDataset.Range.End.Byte,
				End:         existingDataset.Range.End.Byte,
				Replacement: append([]byte("\n"), res.GrantsHCL...),
			},
		})
	}
	return p, nil
}

// planResults resolves every result's placement, keyed by dataset id.
func planResults(repo *tf.Repo, results []*rewrite.Result) (map[string]resultPlacement, error) {
	plans := make(map[string]resultPlacement, len(results))
	for _, res := range results {
		p, err := planResult(repo, res)
		if err != nil {
			return nil, fmt.Errorf("plan %s: %w", res.DatasetID, err)
		}
		plans[res.DatasetID] = p
	}
	return plans, nil
}

// touchedFiles returns every absolute file path any plan would write to, deduplicated — a New
// placement's own file, or every file its edits target. Used for the dry-run preview count, so it
// matches what writeResourceFiles would actually touch rather than counting one file per result,
// which undercounts as soon as a result's grants edit lands in a different file than its dataset,
// and overcounts as soon as two results share a file.
func touchedFiles(plans map[string]resultPlacement) []string {
	seen := map[string]bool{}
	for _, p := range plans {
		if p.New {
			seen[p.DatasetFile] = true
			continue
		}
		for _, e := range p.edits {
			seen[e.file] = true
		}
	}
	files := make([]string, 0, len(seen))
	for f := range seen {
		files = append(files, f)
	}
	return files
}

// writeResourceFiles writes every result's dataset (and, if any, grants) to disk per its plan: a
// whole new file for New placements, or a set of in-place splices for everything else. Returns every
// file path touched, absolute, for the terraform fmt pass that follows.
//
// Edits are grouped by file across every result before any of them are applied, so two results that
// both already live in the same hand-written file — two datasets declared together in one grouped
// .tf file, say — are applied together in a single read-modify-write, rather than as two independent
// writes where the second would either clobber the first or compute its byte offsets against
// already-stale content.
func writeResourceFiles(results []*rewrite.Result, plans map[string]resultPlacement) ([]string, error) {
	var written []string
	edits := map[string][]tf.Edit{}
	for _, res := range results {
		plan := plans[res.DatasetID]
		if plan.New {
			if err := os.WriteFile(plan.DatasetFile, res.HCL, 0o644); err != nil {
				return nil, err
			}
			written = append(written, plan.DatasetFile)
			continue
		}
		for _, e := range plan.edits {
			edits[e.file] = append(edits[e.file], e.edit)
		}
	}

	files := make([]string, 0, len(edits))
	for file := range edits {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		out, err := tf.ApplySplices(src, edits[file])
		if err != nil {
			return nil, fmt.Errorf("update %s: %w", file, err)
		}
		if err := os.WriteFile(file, out, 0o644); err != nil {
			return nil, err
		}
		written = append(written, file)
	}
	return written, nil
}
