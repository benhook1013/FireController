package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestCIWatchHelpQualifiesQuietRearming(t *testing.T) {
	var out, diag bytes.Buffer
	if code := RunContext(t.Context(), []string{"--profiles-file", "/unavailable", "reviews", "ci-wait", "--help"}, nil, &out, &diag); code != ExitSuccess || diag.Len() != 0 {
		t.Fatal("help required a profile or service", code, diag.String())
	}
	var help struct{ Semantics string }
	if err := json.Unmarshal(out.Bytes(), &help); err != nil {
		t.Fatal(err)
	}
	for _, meaning := range []string{"actual PR comparison", "required contexts from the default merge branch", "With --after-observation, an unchanged passed or failed verdict stays quiet", "Without that baseline, any terminal result returns immediately", "Pending checks alone do not wake"} {
		if !strings.Contains(help.Semantics, meaning) {
			t.Fatal("help lost baseline-dependent terminal behavior", help.Semantics)
		}
	}
}
