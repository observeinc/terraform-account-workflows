package tf

import (
	"fmt"
	"strings"
)

// Entry is one indexed state object that an oid can resolve to.
type Entry struct {
	// Address is the terraform address, e.g. `observe_dataset.asv` or
	// `data.observe_datastream.applogs`. Module-relative: the `module.<x>.` prefix is dropped,
	// since references inside a module omit it.
	Address string
	// Type is the terraform resource type, e.g. `observe_dataset`.
	Type string
	// Name is the terraform resource name.
	Name string
	// DatasetName is the Observe object name, used for the replica collision pre-flight.
	DatasetName string
}

// Index holds every oid-addressable object in one state file, bucketed by where it lives so
// reference resolution can prefer a managed peer over a data source.
type Index struct {
	// ManagedInModule maps a normalized oid to a managed resource in the target module.
	ManagedInModule map[string]Entry
	// DataInModule maps a normalized oid to a data source in the target module.
	DataInModule map[string]Entry
	// RootByAddress maps a root-module address (`data.observe_datastream.applogs`) to the oids
	// it exposes, so a module variable binding can be joined to the oid it carries.
	RootByAddress map[string]RootBinding
	// NamesToOID maps an Observe dataset name to its oid, for the replica collision pre-flight.
	NamesToOID map[string]string
	// OIDsByModule maps a module address (`module.piedpiper`) to the oids its managed resources carry,
	// keyed by module-relative address. This is what lets a variable bound to another module's outputs
	// resolve: that module's resources are in state, but outside the target module.
	OIDsByModule map[string]map[string]string
	// GroupNames maps an RBAC group's numeric ID to its name. Built from
	// observe_rbac_group resources (both managed and data) in root scope, where the for_each
	// index_key is the group name. Used to resolve group IDs returned by rbacResourceStatements
	// into var.rbac_groups["<name>"].oid references.
	GroupNames map[string]string
	// GrantsManagedInModule records, by lowercased resource name, whether an observe_resource_grants
	// resource with that name is already in state in the target module.
	//
	// This can't be answered from ManagedInModule: a grants resource's own `oid` attribute is set to
	// the dataset's oid it grants against, not a separate identity, and identityOIDs only ever
	// indexes one entry per oid — state's alphabetical ordering means observe_dataset is read before
	// observe_resource_grants, so the dataset's own entry always wins that oid and the grants
	// resource's presence has to be tracked by name instead.
	GrantsManagedInModule map[string]bool
	// WorkspaceOID is the workspace every dataset in the module belongs to.
	WorkspaceOID string
}

// RootBinding records the oids a root-module object exposes. A datastream carries both its own
// `oid` and the `dataset` oid it writes into, and module variables reference either.
type RootBinding struct {
	Address    string
	OID        string
	DatasetOID string
	// Outputs is an `outputs` attribute holding a JSON document, as `observe_app` exposes. The repo
	// binds module variables to `jsondecode(data.observe_app.X.outputs)`, so the oids an app installed
	// are reachable only by decoding this — nothing about the data source's own oid describes them.
	Outputs string
}

// identityOIDs returns the oids that identify this resource itself, so nothing gets indexed under an
// oid it merely points at.
//
// `oid` is always the resource's own. `dataset` is only its own for a datastream, where it names the
// dataset the stream writes into and a module variable may legitimately reference either. On anything
// else `dataset` is a reference: `observe_correlation_tag.dataset` is the dataset being tagged and
// `observe_reference_table.dataset` is the dataset holding the table's rows.
//
// Indexing those references cost a dataset its own entry. Terraform state is ordered alphabetically by
// type, so `observe_correlation_tag` is always read before `observe_dataset`, and with first-writer-wins
// the tag kept the key — deterministically, for 26 of the reference repo's 661 managed datasets. Build
// then skips the entry because its type is not `observe_dataset`, so the dataset resolves to a
// dashboard-scoped data source or, with no data source at all, comes back unwired: a report telling the
// operator to wire up a module variable for a dataset already managed a few files away.
//
// `observe_reference_table` escaped that only by sorting after `observe_dataset`, which is why this is
// keyed on what an attribute means rather than on which types happen to collide today.
func identityOIDs(resourceType, selfOID, datasetOID string) []string {
	oids := make([]string, 0, 2)
	if selfOID != "" {
		oids = append(oids, selfOID)
	}
	if datasetOID != "" && resourceType == "observe_datastream" {
		oids = append(oids, datasetOID)
	}
	return oids
}

// BuildIndex indexes state for the given module address, e.g. `module.example`.
func BuildIndex(s *State, moduleAddress string) *Index {
	idx := &Index{
		ManagedInModule:       map[string]Entry{},
		DataInModule:          map[string]Entry{},
		RootByAddress:         map[string]RootBinding{},
		NamesToOID:            map[string]string{},
		OIDsByModule:          map[string]map[string]string{},
		GroupNames:            map[string]string{},
		GrantsManagedInModule: map[string]bool{},
	}

	for i := range s.Resources {
		r := &s.Resources[i]
		attrs := r.attrs()
		if attrs == nil {
			continue
		}

		// A dataset's own oid comes from either `oid` or, for a datastream, the `dataset` it
		// writes into. Both are worth indexing: a module variable may reference either.
		selfOID := NormalizeOID(str(attrs, "oid"))
		datasetOID := NormalizeOID(str(attrs, "dataset"))
		name := str(attrs, "name")

		if r.Type == "observe_dataset" {
			for _, o := range []string{selfOID, datasetOID} {
				if o != "" && name != "" {
					if _, exists := idx.NamesToOID[name]; !exists {
						idx.NamesToOID[name] = o
					}
				}
			}
		}

		if r.Mode == "managed" && r.Module != "" && r.Module != moduleAddress && selfOID != "" {
			if idx.OIDsByModule[r.Module] == nil {
				idx.OIDsByModule[r.Module] = map[string]string{}
			}
			if _, exists := idx.OIDsByModule[r.Module][address(r)]; !exists {
				idx.OIDsByModule[r.Module][address(r)] = selfOID
			}
		}

		switch r.Module {
		case moduleAddress:
			entry := Entry{
				Address:     address(r),
				Type:        r.Type,
				Name:        r.Name,
				DatasetName: name,
			}
			target := idx.DataInModule
			if r.Mode == "managed" {
				target = idx.ManagedInModule
			}
			for _, o := range identityOIDs(r.Type, selfOID, datasetOID) {
				// First writer wins so index order is stable and a later duplicate (two
				// data sources on the same dataset) does not shadow the first.
				if _, exists := target[o]; !exists {
					target[o] = entry
				}
			}
			if r.Type == "observe_workspace" && idx.WorkspaceOID == "" {
				idx.WorkspaceOID = selfOID
			}
			if r.Type == "observe_resource_grants" && r.Mode == "managed" {
				idx.GrantsManagedInModule[strings.ToLower(r.Name)] = true
			}
		case "":
			b := RootBinding{
				Address:    address(r),
				OID:        selfOID,
				DatasetOID: datasetOID,
				Outputs:    str(attrs, "outputs"),
			}
			if b.OID != "" || b.DatasetOID != "" || b.Outputs != "" {
				idx.RootByAddress[b.Address] = b
			}
			if r.Type == "observe_workspace" && idx.WorkspaceOID == "" {
				idx.WorkspaceOID = selfOID
			}

			// RBAC groups in root scope use for_each keyed by group name. Each instance's
			// index_key is the name, and attributes.id is the numeric id.
			if r.Type == "observe_rbac_group" {
				for _, inst := range r.Instances {
					name, _ := inst.IndexKey.(string)
					id := str(inst.Attributes, "id")
					if name != "" && id != "" {
						idx.GroupNames[id] = name
					}
				}
			}
		}
	}

	return idx
}

// address renders a resource's terraform address without any module prefix, which is the form both a
// module-relative reference and a root-module address take.
func address(r *Resource) string {
	if r.Mode == "data" {
		return fmt.Sprintf("data.%s.%s", r.Type, r.Name)
	}
	return fmt.Sprintf("%s.%s", r.Type, r.Name)
}
