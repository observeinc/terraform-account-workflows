package observe

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// rbacResourceStatementsQuery fetches RBAC v2 statements scoped to a resource.
//
// The response fragment matches the provider's RbacStatement fragment: id, subject (userId/groupId/all),
// object (objectId/type/all/owner), role, version.
const rbacResourceStatementsQuery = `query GetRbacResourceStatements($ids: [ObjectId!]!) {
  rbacResourceStatements(ids: $ids) {
    id
    subject {
      userId
      groupId
      all
    }
    object {
      objectId
      type
      all
      owner
    }
    role
    version
  }
}`

// RbacStatement is the GQL response shape.
type RbacStatement struct {
	ID      string      `json:"id"`
	Subject rbacSubject `json:"subject"`
	Object  rbacObject  `json:"object"`
	Role    string      `json:"role"`
	Version *int        `json:"version"`
}

type rbacSubject struct {
	UserID  *string `json:"userId"`
	GroupID *string `json:"groupId"`
	All     *bool   `json:"all"`
}

type rbacObject struct {
	ObjectID *string `json:"objectId"`
	Type     *string `json:"type"`
	All      *bool   `json:"all"`
	Owner    *bool   `json:"owner"`
}

// DatasetGrant is one group-scoped grant on a dataset, ready for HCL generation.
type DatasetGrant struct {
	GroupID string
	Role    string // terraform role name, e.g. "dataset_viewer"
}

// GetDatasetGrants fetches the RBAC statements for a dataset and returns the group-scoped grants in
// their Terraform representation.
//
// User grants and all-subject grants are dropped: only group grants matter for the generated
// observe_resource_grants block.
func (c *Client) GetDatasetGrants(ctx context.Context, datasetID string) ([]DatasetGrant, error) {
	var out struct {
		RbacResourceStatements []RbacStatement `json:"rbacResourceStatements"`
	}
	if err := c.Query(ctx, rbacResourceStatementsQuery, map[string]any{"ids": []string{datasetID}}, &out); err != nil {
		return nil, fmt.Errorf("rbacResourceStatements(%s): %w", datasetID, err)
	}

	var grants []DatasetGrant
	for _, stmt := range out.RbacResourceStatements {
		// Only group subjects.
		if stmt.Subject.GroupID == nil {
			continue
		}
		role := flattenRole(stmt.Role, stmt.Object)
		if role == "" {
			continue
		}
		grants = append(grants, DatasetGrant{
			GroupID: *stmt.Subject.GroupID,
			Role:    role,
		})
	}

	// Deterministic order: by role then group ID, so the output is stable across runs.
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Role != grants[j].Role {
			return grants[i].Role < grants[j].Role
		}
		return grants[i].GroupID < grants[j].GroupID
	})
	return grants, nil
}

// flattenRole converts a GQL RbacStatement into the Terraform grant role string.
//
// This reproduces the relevant subset of the provider's flattenRoleAndObject for resource-scoped
// statements. Only dataset-typed viewer/editor are expected from rbacResourceStatements on a dataset,
// but the logic handles the full set the provider does.
func flattenRole(gqlRole string, obj rbacObject) string {
	// Normalize: the GQL enum is title-case ("Viewer"), Terraform uses snake_case.
	role := strings.ToLower(gqlRole)

	if obj.Type == nil {
		return ""
	}
	objType := *obj.Type

	if obj.ObjectID != nil {
		// Resource-scoped viewer/editor.
		switch {
		case role == "viewer":
			return viewGrantRoleForType(objType)
		case role == "editor":
			return editGrantRoleForType(objType)
		}
	} else {
		// Type-scoped editor without an object ID is a creator role.
		if role == "editor" {
			return createGrantRoleForType(objType)
		}
	}
	return ""
}

func viewGrantRoleForType(objType string) string {
	switch objType {
	case "dataset":
		return "dataset_viewer"
	case "datastream":
		return "datastream_viewer"
	case "dashboard":
		return "dashboard_viewer"
	case "monitor":
		return "monitor_viewer"
	case "worksheet":
		return "worksheet_viewer"
	case "aichat":
		return "aichat_viewer"
	}
	return ""
}

func editGrantRoleForType(objType string) string {
	switch objType {
	case "dataset":
		return "dataset_editor"
	case "datastream":
		return "datastream_editor"
	case "dashboard":
		return "dashboard_editor"
	case "monitor":
		return "monitor_editor"
	case "worksheet":
		return "worksheet_editor"
	case "aichat":
		return "aichat_editor"
	}
	return ""
}

func createGrantRoleForType(objType string) string {
	switch objType {
	case "dataset":
		return "dataset_creator"
	case "datastream":
		return "datastream_creator"
	case "dashboard":
		return "dashboard_creator"
	case "monitor":
		return "monitor_creator"
	case "worksheet":
		return "worksheet_creator"
	case "aichat":
		return "aichat_creator"
	}
	return ""
}
