package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

type projectPermissions struct{ read, write, manage int }

func permissionSet(values []string) (projectPermissions, []string, error) {
	var p projectPermissions
	invalid := func() (projectPermissions, []string, error) {
		return projectPermissions{}, nil, core.Fail("validation_failed", "Permissions must be unique read/write/manage values and include read or manage.", 400)
	}
	if len(values) > 3 {
		return invalid()
	}
	for _, value := range values {
		switch value {
		case "read":
			if p.read != 0 {
				return invalid()
			}
			p.read = 1
		case "write":
			if p.write != 0 {
				return invalid()
			}
			p.write = 1
		case "manage":
			if p.manage != 0 {
				return invalid()
			}
			p.manage = 1
		default:
			return invalid()
		}
	}
	if p.manage == 1 {
		p.read, p.write = 1, 1
	}
	if p.read != 1 {
		return invalid()
	}
	return p, p.values(), nil
}

func (p projectPermissions) values() []string {
	values := []string{}
	if p.read == 1 {
		values = append(values, "read")
	}
	if p.write == 1 {
		values = append(values, "write")
	}
	if p.manage == 1 {
		values = append(values, "manage")
	}
	return values
}

func (a *api) grant(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	var request struct {
		TokenSHA256 string   `json:"token_sha256"`
		Permissions []string `json:"permissions"`
	}
	if err = core.ReadJSON(w, r, &request); err != nil {
		core.WriteError(w, err)
		return
	}
	hash, err := hex.DecodeString(request.TokenSHA256)
	if err != nil || len(hash) != sha256.Size || request.TokenSHA256 != strings.ToLower(request.TokenSHA256) {
		core.WriteError(w, core.Fail("validation_failed", "Provide the lowercase SHA-256 hash of a client credential.", 400))
		return
	}
	permissions, normalized, err := permissionSet(request.Permissions)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	request.Permissions = normalized
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	result, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "credentials.grant", Target: "credentials", Key: k, Request: request}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var existing string
		err := tx.QueryRowContext(ctx, "SELECT id FROM credentials WHERE token_sha256=?", request.TokenSHA256).Scan(&existing)
		if err == nil {
			return nil, core.Fail("credential_already_configured", "Use a distinct client credential for a new grant.", 409)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		id, principal := core.NewID("credential"), core.NewID("principal")
		if _, err = tx.ExecContext(ctx, "INSERT INTO credentials(id,principal_id,token_sha256,is_admin,revoked) VALUES(?,?,?,0,0)", id, principal, request.TokenSHA256); err != nil {
			return nil, err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO grants(credential_id,project_id,can_read,can_write,can_manage) VALUES(?,?,?,?,?)", id, scope.ProjectID, permissions.read, permissions.write, permissions.manage); err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "credentials.granted", id, 1, map[string]any{"id": id, "principal_id": principal, "permissions": request.Permissions}); err != nil {
			return nil, err
		}
		return map[string]string{"id": id, "principal_id": principal, "project_id": scope.ProjectID, "outcome": "granted"}, nil
	})
	complete(w, result, err)
}

func (a *api) replaceGrant(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	if _, err = core.QueryKeys(r, "validation_failed"); err != nil {
		core.WriteError(w, err)
		return
	}
	var wire struct {
		ExpectedPermissions *[]string `json:"expected_permissions"`
		Permissions         *[]string `json:"permissions"`
	}
	if err = core.ReadJSON(w, r, &wire); err != nil {
		core.WriteError(w, err)
		return
	}
	if wire.ExpectedPermissions == nil || wire.Permissions == nil {
		core.WriteError(w, core.Fail("validation_failed", "Provide expected_permissions and permissions arrays.", 400))
		return
	}
	_, expected, err := permissionSet(*wire.ExpectedPermissions)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	next, permissions, err := permissionSet(*wire.Permissions)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("credential_id")
	input := struct {
		ExpectedPermissions []string `json:"expected_permissions"`
		Permissions         []string `json:"permissions"`
	}{expected, permissions}
	value, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "credentials.permissions.replace", Target: id, Key: k, Request: input}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var previous projectPermissions
		err := tx.QueryRowContext(ctx, `SELECT g.can_read,g.can_write,g.can_manage FROM grants g JOIN credentials c ON c.id=g.credential_id WHERE c.id=? AND g.project_id=? AND c.is_admin=0 AND c.revoked=0 AND g.can_read=1`, id, scope.ProjectID).Scan(&previous.read, &previous.write, &previous.manage)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, core.Fail("not_found", "The active credential grant is unavailable in this project.", 404)
		}
		if err != nil {
			return nil, err
		}
		current := previous.values()
		if !slices.Equal(current, expected) {
			return nil, &core.Error{Code: "credential_permissions_changed", Message: "The credential permissions changed; compare the returned current_permissions with the intended change before retrying.", Status: 409, Details: map[string]any{"credential_id": id, "current_permissions": current}}
		}
		outcome := "unchanged"
		if !slices.Equal(current, permissions) {
			if _, err = tx.ExecContext(ctx, `UPDATE grants SET can_read=?,can_write=?,can_manage=? WHERE credential_id=? AND project_id=?`, next.read, next.write, next.manage, id, scope.ProjectID); err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "credentials.permissions_changed", id, 1, map[string]any{"id": id, "previous_permissions": current, "permissions": permissions}); err != nil {
				return nil, err
			}
			outcome = "updated"
		}
		return map[string]any{"id": id, "project_id": scope.ProjectID, "outcome": outcome, "permissions": permissions}, nil
	})
	complete(w, value, err)
}

func (a *api) revoke(w http.ResponseWriter, r *http.Request) {
	scope, err := a.store.AdminScope(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	k, err := key(r)
	if err != nil {
		core.WriteError(w, err)
		return
	}
	id := r.PathValue("credential_id")
	result, err := a.store.Mutate(r.Context(), scope, core.Mutation{Operation: "credentials.revoke", Target: id, Key: k, Request: struct{}{}}, func(ctx context.Context, tx *sql.Tx) (any, error) {
		var revoked, read, write, manage int
		err := tx.QueryRowContext(ctx, "SELECT c.revoked,g.can_read,g.can_write,g.can_manage FROM credentials c JOIN grants g ON g.credential_id=c.id WHERE c.id=? AND g.project_id=? AND c.is_admin=0", id, scope.ProjectID).Scan(&revoked, &read, &write, &manage)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, core.Fail("not_found", "The credential is unavailable in this project.", 404)
		}
		if err != nil {
			return nil, err
		}
		if revoked == 0 && (read != 0 || write != 0 || manage != 0) {
			// This URL revokes only its project's authority. A credential may
			// retain a grant elsewhere; never revoke another project's access.
			if _, err = tx.ExecContext(ctx, "UPDATE grants SET can_read=0,can_write=0,can_manage=0 WHERE credential_id=? AND project_id=?", id, scope.ProjectID); err != nil {
				return nil, err
			}
			if _, err = tx.ExecContext(ctx, "UPDATE credentials SET revoked=1 WHERE id=? AND NOT EXISTS(SELECT 1 FROM grants WHERE credential_id=? AND (can_read=1 OR can_write=1 OR can_manage=1))", id, id); err != nil {
				return nil, err
			}
			if err = core.AppendEvent(ctx, tx, scope, "credentials.revoked", id, 1, map[string]string{"id": id}); err != nil {
				return nil, err
			}
		}
		return map[string]string{"id": id, "project_id": scope.ProjectID, "outcome": "revoked"}, nil
	})
	complete(w, result, err)
}
