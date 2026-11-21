package server

import (
	"cmp"
	"context"
	"database/sql"
	"net/http"
	"slices"

	"github.com/benhook1013/FireController/internal/core"
)

type connectorProjectGrant struct {
	ProjectID   string   `json:"project_id"`
	WorkerID    string   `json:"worker_id"`
	Permissions []string `json:"permissions"`
}

func (a *api) grantMultiProjectConnector(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct {
		TokenSHA256 string                  `json:"token_sha256"`
		Projects    []connectorProjectGrant `json:"projects"`
	}
	if err = core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	if !validCredentialHash(request.TokenSHA256) || len(request.Projects) < 2 || len(request.Projects) > core.MaxConnectorProjects {
		core.WriteError(w, core.Fail("validation_failed", "Provide a lowercase SHA-256 credential digest and two to sixteen project grants.", 400))
		return
	}
	slices.SortFunc(request.Projects, func(a, b connectorProjectGrant) int {
		return cmp.Compare(a.ProjectID, b.ProjectID)
	})
	for i := range request.Projects {
		grant := &request.Projects[i]
		permissions, normalized, err := permissionSet(grant.Permissions)
		if err != nil || permissions.manage != 0 || !boundedText(grant.ProjectID, 128) || !boundedText(grant.WorkerID, 128) || i > 0 && grant.ProjectID == request.Projects[i-1].ProjectID {
			core.WriteError(w, core.Fail("validation_failed", "Select one distinct project ID, an active worker ID, and read or read/write permissions for each grant.", 400))
			return
		}
		grant.Permissions = normalized
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "credentials.connector.grant", Target: "credentials", Key: k, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		id, principal, err := createGrantCredential(ctx, tx, request.TokenSHA256)
		if err != nil {
			return nil, err
		}
		for _, grant := range request.Projects {
			if err = core.CheckActiveWorker(ctx, tx, grant.ProjectID, grant.WorkerID); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, "INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES(?,?,1,?,0)", id, grant.ProjectID, slices.Contains(grant.Permissions, "write")); err != nil {
				return nil, err
			}
			if err = core.BindConnectorCredential(ctx, tx, id, grant.ProjectID, grant.WorkerID); err != nil {
				return nil, err
			}
			projectScope := scope
			projectScope.ProjectID = grant.ProjectID
			event := map[string]any{"id": id, "principal_id": principal, "permissions": grant.Permissions, "connector_worker_id": grant.WorkerID}
			if err = core.AppendEvent(ctx, tx, projectScope, "credentials.granted", id, 1, event); err != nil {
				return nil, err
			}
		}
		return map[string]any{"id": id, "principal_id": principal, "projects": request.Projects, "outcome": "granted"}, nil
	})
	complete(w, result, err)
}
