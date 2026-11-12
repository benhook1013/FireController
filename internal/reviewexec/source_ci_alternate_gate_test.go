package reviewexec

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestSourceCINonstrictAlternateGatePreservesPRComparison(t *testing.T) {
	target := reviewprovider.CITarget{RepositoryID: 801, Repository: "owner/repo", PullNumber: 2, HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), BaseRef: "feature/parent"}
	for _, state := range []string{"satisfied", "pending", "failed", "not_required"} {
		t.Run(state, func(t *testing.T) {
			o := sourceCINonstrictHeadObservation(target, state)
			o.Authority.Branch = "main"
			source := SourceObservation{RepositoryID: target.RepositoryID, RepositoryName: target.Repository, PullNumber: target.PullNumber, HeadSHA: target.HeadSHA, BaseSHA: target.BaseSHA, BaseRef: target.BaseRef, ObservedAt: o.StartedAt.Format(time.RFC3339Nano)}
			before := encoded(o)
			if !reviewprovider.NonstrictHeadCICandidate(*o.Candidate, target, o.Authority) || !validCIWithAuthorityBranch(o, source, "main", false) {
				t.Fatal("alternate gate incorrectly rejected pinned nonstrict head proof", state)
			}
			if !reflect.DeepEqual(o.Target, target) || o.Candidate.BaseRef != target.BaseRef || o.Candidate.BaseSHA != target.BaseSHA || encoded(o) != before {
				t.Fatal("gate validation rewrote pinned PR comparison")
			}
			if validCIWithAuthorityBranch(o, source, "other", false) || validSourceCI(o, source) {
				t.Fatal("authority from wrong gate branch was accepted")
			}
			o.Authority.Strict = true
			if validCIWithAuthorityBranch(o, source, "main", false) {
				t.Fatal("strict authority allowed stale nonstrict comparison")
			}
			o.Authority.Strict = false
			o.Target.BaseRef = "main"
			if validCIWithAuthorityBranch(o, source, "main", false) {
				t.Fatal("rewritten target escaped exact source binding")
			}
			o.Target = target
			o.Candidate.BaseSHA = strings.Repeat("f", 40)
			if validCIWithAuthorityBranch(o, source, "main", false) {
				t.Fatal("wrong candidate base escaped pinned comparison")
			}
		})
	}
}
