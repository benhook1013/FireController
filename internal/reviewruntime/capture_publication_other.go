//go:build !linux

package reviewruntime

import "context"

type unleasedPublication struct{}

func (unleasedPublication) Validate() error { return nil }
func (unleasedPublication) Close() error    { return nil }
func capturePublication(context.Context, string) (publicationCustody, error) {
	return unleasedPublication{}, nil
}
func captureStagingPattern(kind string) string {
	switch kind {
	case "hosted":
		return ".hosted-capture-*"
	case "exhaustion":
		return ".exhaustion-*"
	default:
		return ".capture-*"
	}
}
