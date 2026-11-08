package reviewruntime

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/benhook1013/FireController/internal/core"
	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

func TestHostedRoundClosurePreservesQuietDispatchReason(t *testing.T) {
	f := newHostedDriverFixture(t)
	runtimeRoundGrant(t, f.cliDriverFixture, 2, "hosted", 3)
	op, a := f.prepareHosted(t)
	revision := int64(1)
	if _, err := f.engine.ConfigureRoundControl(t.Context(), f.scope(t, f.admin, true), "close-before-hosted-dispatch", 2, "hosted", reviewexec.RoundControlRequest{ExpectedRevision: &revision, Action: "close"}); err != nil {
		t.Fatal(err)
	}
	result, err := f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
	if err != nil || result.State != "failed" || result.Reason != "review_round_closed" || f.provider.reviews != 0 {
		t.Fatal("Hosted closure lost quiet classification or posted", result, err)
	}
}

func TestFinalActionQuietReasonsAreFinite(t *testing.T) {
	for _, reason := range []string{"pull_draft", "target_already_covered", "hosted_chat_cooldown"} {
		if got := actionRefusalReason(reviewHold(reason), "fallback"); got != reason {
			t.Fatal("known quiet hold flattened", reason, got)
		}
	}
	for _, failure := range []error{errors.New("pull_draft private"), reviewHold("private_unqualified"), &core.Error{Code: "other", Details: map[string]string{"reason": "pull_draft"}}, &reviewprovider.Error{Code: "private_code", Message: "provider_rate_limited private"}} {
		if got := actionRefusalReason(failure, "fallback"); got != "fallback" {
			t.Fatal("arbitrary diagnostic became a reason", got)
		}
	}
}

func TestFinalSourceRateLimitBothChannelsRetainsClassificationWithoutDispatch(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			failure := &reviewprovider.Error{Code: "provider_rate_limited", Status: 429, Message: "private raw response"}
			var result reviewexec.Result
			var err error
			if channel == "hosted" {
				f := newHostedDriverFixture(t)
				op, a := f.prepareHosted(t)
				f.hosted.provider = preparationFailingHosted{HostedProvider: f.provider, failure: failure}
				result, err = f.hosted.Dispatch(t.Context(), op, a, f.callback(t, &a))
				if f.provider.reviews != 0 {
					t.Fatal("throttled source posted a review")
				}
			} else {
				f := newCLIDriverFixture(t)
				op, a := f.prepareAdmit(t, 2, 1, "ready-before-source-rate-limit")
				f.driver.reader = preparationFailingSource{TargetReader: f.reader, failure: failure}
				result, err = f.driver.Dispatch(t.Context(), op, a, nil)
				log, readErr := os.ReadFile(f.log)
				if readErr != nil || strings.Contains(string(log), "review ") {
					t.Fatal("throttled source launched CLI", readErr)
				}
			}
			if err != nil || result.State != "failed" || result.Reason != "provider_rate_limited" {
				t.Fatal("typed 429 lost its stable class", result, err)
			}
		})
	}
}
