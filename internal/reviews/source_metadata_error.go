package reviews

// SourceMetadataError identifies malformed display facts for one retained PR.
// It never describes a failed SQL read or corruption of shared history authority.
// The wrapped public error preserves the existing fail-closed API response.
type SourceMetadataError struct {
	ProjectID  string
	PullNumber int64
	cause      error
}

func (e *SourceMetadataError) Error() string { return e.cause.Error() }
func (e *SourceMetadataError) Unwrap() error { return e.cause }

func validateSourceMetadataBody(project string, number int64, body []any) error {
	if len(body) > 12 {
		raw, rawOK := body[12].(string)
		state, stateOK := body[1].(string)
		if rawOK && stateOK && oneOf(state, "open", "closed", "merged", "unknown") {
			if _, err := ReadPullMetadata(raw, state); err == nil {
				return nil
			}
		}
	}
	return &SourceMetadataError{ProjectID: project, PullNumber: number, cause: historyInconsistent()}
}
