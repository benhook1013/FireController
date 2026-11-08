package reviewruntime

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewexec"
	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type preparationFailingSource struct {
	reviewexec.TargetReader
	failure error
}

func (r preparationFailingSource) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{}, r.failure
}

type preparationFailingHosted struct {
	HostedProvider
	failure error
}

func (r preparationFailingHosted) Repository(context.Context, int64) (reviewprovider.Repository, error) {
	return reviewprovider.Repository{}, r.failure
}

func TestNativePreparationFailureAtSourceBeforeProviderAction(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			failure := &reviewprovider.Error{Code: "provider_unauthorized", Message: "/private/path ghp_secret provider response"}
			var err error
			var f *cliDriverFixture
			if channel == "hosted" {
				hosted := newHostedDriverFixture(t)
				f = hosted.cliDriverFixture
				op, a, c := hosted.startHosted(t)
				hosted.hosted.provider = preparationFailingHosted{HostedProvider: hosted.provider, failure: failure}
				_, err = hosted.hosted.Prepare(t.Context(), op, c, a)
				if len(hosted.hosted.prepared) != 0 {
					t.Fatal("failed preparation leaked permit")
				}
				if hosted.provider.queries != 0 || hosted.provider.reviews != 0 {
					t.Fatal("failed source invoked Hosted allowance or review")
				}
			} else {
				f = newCLIDriverFixture(t)
				op, a, c := f.start(t, 2, 1, "source-failure")
				f.driver.reader = preparationFailingSource{TargetReader: f.reader, failure: failure}
				_, err = f.driver.Prepare(t.Context(), op, c, a)
				if len(f.driver.prepared) != 0 {
					t.Fatal("failed preparation leaked permit")
				}
			}
			if err == nil || err.Error() != "preparation_source_resolution_provider_unauthorized" || !errors.Is(err, failure) {
				t.Fatalf("missing safe source attribution: %v", err)
			}
			if f.fetches.Load() != 0 {
				t.Fatal("failed source invoked allowance, review or Git")
			}
			if _, err = os.Stat(f.log); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("failed source ran CLI", err)
			}
		})
	}
}
