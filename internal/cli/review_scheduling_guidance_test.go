package cli

import (
	"strings"
	"testing"
)

func TestSchedulingConfigureHelpDistinguishesValueFromAuthorizer(t *testing.T) {
	info, ok := operationHelp("reviews scheduling configure")
	if !ok {
		t.Fatal("configure help missing")
	}
	for _, phrase := range []string{"preserves the setting, not the authorizer", "shared authorizer for new work", "even when disabling only the other channel"} {
		if !strings.Contains(strings.Join(info.Semantics, " "), phrase) {
			t.Errorf("configure help is missing %q", phrase)
		}
	}
}
