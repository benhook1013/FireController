package core

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
)

type cursorValue struct{ Instance, Principal, Project, Filter, After string }

func (s *Store) Cursor(scope Scope, filter, after string) string {
	data, _ := json.Marshal(cursorValue{s.instanceID, scope.PrincipalID, scope.ProjectID, filter, after})
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(data) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *Store) ParseCursor(scope Scope, filter, cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	invalid := Fail("validation_failed", "The cursor does not match this instance, principal, project and filter.", 400)
	if len(cursor) > 4096 {
		return "", invalid
	}
	parts := strings.Split(cursor, ".")
	if len(parts) != 2 {
		return "", invalid
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return "", invalid
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", invalid
	}
	mac := hmac.New(sha256.New, s.signingKey)
	_, _ = mac.Write(data)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", invalid
	}
	var value cursorValue
	if json.Unmarshal(data, &value) != nil || value.Instance != s.instanceID || value.Principal != scope.PrincipalID || value.Project != scope.ProjectID || value.Filter != filter {
		return "", invalid
	}
	return value.After, nil
}
