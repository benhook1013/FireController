package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/benhook1013/FireController/internal/core"
)

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
	read, write := 0, 0
	for _, permission := range request.Permissions {
		switch permission {
		case "read":
			if read != 0 {
				core.WriteError(w, core.Fail("validation_failed", "Permissions must be unique read/write values.", 400))
				return
			}
			read = 1
		case "write":
			if write != 0 {
				core.WriteError(w, core.Fail("validation_failed", "Permissions must be unique read/write values.", 400))
				return
			}
			write = 1
		default:
			core.WriteError(w, core.Fail("validation_failed", "Permissions must be unique read/write values.", 400))
			return
		}
	}
	if read != 1 {
		core.WriteError(w, core.Fail("validation_failed", "A project credential requires read permission.", 400))
		return
	}
	// Canonical ordering makes equivalent permission sets the same request.
	request.Permissions = []string{"read"}
	if write == 1 {
		request.Permissions = append(request.Permissions, "write")
	}
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
		if _, err = tx.ExecContext(ctx, "INSERT INTO grants(credential_id,project_id,can_read,can_write) VALUES(?,?,?,?)", id, scope.ProjectID, read, write); err != nil {
			return nil, err
		}
		if err = core.AppendEvent(ctx, tx, scope, "credentials.granted", id, 1, map[string]string{"id": id, "principal_id": principal}); err != nil {
			return nil, err
		}
		return map[string]string{"id": id, "principal_id": principal, "project_id": scope.ProjectID, "outcome": "granted"}, nil
	})
	complete(w, result, err)
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
		var revoked int
		err := tx.QueryRowContext(ctx, "SELECT c.revoked FROM credentials c JOIN grants g ON g.credential_id=c.id WHERE c.id=? AND g.project_id=? AND c.is_admin=0", id, scope.ProjectID).Scan(&revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, core.Fail("not_found", "The credential is unavailable in this project.", 404)
		}
		if err != nil {
			return nil, err
		}
		if revoked == 0 {
			if _, err = tx.ExecContext(ctx, "UPDATE credentials SET revoked=1 WHERE id=?", id); err != nil {
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
