package server

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

// GitHubHistoryReader uses the explicitly selected repository credential file.
// An empty selection permits public reads. It performs no provider request,
// executable discovery or credential lookup outside that one file.
func GitHubHistoryReader(tokenFile string) (*reviewprovider.GitHub, string, error) {
	token, err := loadGitHubCredential(tokenFile)
	if err != nil {
		return nil, "", err
	}
	reader, err := reviewprovider.NewGitHub(token)
	if err != nil {
		return nil, "", core.Fail("configuration_invalid", "The GitHub history reader configuration is invalid.", 500)
	}
	digest := sha256.Sum256([]byte("github-history-reader:v1\x00" + token))
	return reader, hex.EncodeToString(digest[:]), nil
}
