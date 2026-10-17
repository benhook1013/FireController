// Package roundadoptionmodel defines prepared operational configuration without
// storage, provider, or source-controller dependencies.
package roundadoptionmodel

type Prepared struct {
	Version          int    `json:"version"`
	RepositoryID     int64  `json:"repository_id"`
	ExpectedRevision *int64 `json:"expected_revision"`
	PullNumber       int64  `json:"pull_number"`
	Channel          string `json:"channel"`
	Source           string `json:"source"`
	SourceKey        string `json:"source_key"`
	SourceSHA256     string `json:"source_sha256"`
	MinimumRemaining int64  `json:"minimum_remaining"`
	MaximumRemaining int64  `json:"maximum_remaining"`
	InitialDry       int64  `json:"initial_dry"`
	Taper            int64  `json:"taper"`
	Tapered          bool   `json:"tapered"`
	Closed           bool   `json:"closed"`
}

type ApplyRequest struct {
	PreviewSHA256 string   `json:"preview_sha256"`
	Control       Prepared `json:"control"`
}

type Preview struct {
	ProjectID     string `json:"project_id"`
	PullNumber    int64  `json:"pull_number"`
	Channel       string `json:"channel"`
	PayloadSHA256 string `json:"payload_sha256"`
	PreviewSHA256 string `json:"preview_sha256"`
}
