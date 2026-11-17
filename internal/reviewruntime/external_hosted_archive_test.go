package reviewruntime

import (
	"context"
	"reflect"
	"testing"

	"github.com/benhook1013/FireController/internal/reviewprovider"
)

type archiveScopedObservationProvider struct {
	HostedProvider
	calls, strictCalls int
	repository         string
	number             int64
	actor              reviewprovider.Author
	result             reviewprovider.HostedPublicationSnapshot
}

func (p *archiveScopedObservationProvider) ObserveHosted(context.Context, string, int64) (reviewprovider.HostedPublicationSnapshot, error) {
	p.strictCalls++
	return reviewprovider.HostedPublicationSnapshot{}, nil
}
func (p *archiveScopedObservationProvider) ObserveHostedArchive(_ context.Context, repository string, number int64, actor reviewprovider.Author) (reviewprovider.HostedPublicationSnapshot, error) {
	p.calls++
	p.repository, p.number, p.actor = repository, number, actor
	return p.result, nil
}
func TestHostedArchiveSourceUsesOnlyConfiguredActorScopedRead(t *testing.T) {
	actor := reviewprovider.Author{ID: 20, Login: "coderabbitai[bot]", Kind: "Bot"}
	p := &archiveScopedObservationProvider{result: reviewprovider.HostedPublicationSnapshot{Complete: true, IssueComments: []reviewprovider.IssueComment{}, Reviews: []reviewprovider.HostedReview{}, ReviewComments: []reviewprovider.ReviewComment{}}}
	r := &hostedArchiveSource{provider: p, actor: actor}
	got, err := r.ObserveHosted(t.Context(), "owner/repo", 42)
	if err != nil || !reflect.DeepEqual(got, p.result) || p.calls != 1 || p.strictCalls != 0 || p.repository != "owner/repo" || p.number != 42 || p.actor != actor {
		t.Fatal("archive read lost original target/actor or used native collection", err, p.calls, p.strictCalls)
	}
}
