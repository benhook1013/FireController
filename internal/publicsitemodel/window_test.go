package publicsitemodel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func windowRounds(start, count int) []RoundFacts {
	rounds := make([]RoundFacts, count)
	for i := range rounds {
		rounds[i] = publicRoundFact(start + i)
	}
	return rounds
}

func TestPublicWindowClipsValidatedCandidatesWithoutChangingInputs(t *testing.T) {
	for _, count := range []int{101, 200} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			manifest := approvedManifest()
			duration := int64(1234)
			facts := []ReviewFacts{{Number: 2, Lifecycle: "merged", CLI: windowRounds(1, count), Hosted: windowRounds(1001, count)}}
			facts[0].CLI[0].DurationMilliseconds = &duration
			before, _ := json.Marshal(struct {
				Manifest Manifest
				Facts    []ReviewFacts
			}{manifest, facts})
			if _, err := BuildContent(manifest, facts); err == nil {
				t.Fatal("strict retained builder accepted over100 rounds")
			}
			got, err := BuildWindowContent(manifest, facts)
			if err != nil {
				t.Fatal(err)
			}
			review := got.Reviews[1]
			if len(review.CLI) != 100 || len(review.Hosted) != 100 || !review.CLIHasMore || !review.HostedHasMore || review.CLI[0].Key != facts[0].CLI[count-100].Key || review.Hosted[99].Key != facts[0].Hosted[count-1].Key || review.Lifecycle != "merged" {
				t.Fatal("count window lost selected latest summaries/flags")
			}
			after, _ := json.Marshal(struct {
				Manifest Manifest
				Facts    []ReviewFacts
			}{manifest, facts})
			if !bytes.Equal(before, after) {
				t.Fatal("window construction mutated inputs")
			}
			raw, _ := json.Marshal(got)
			if _, err := DecodeContent(raw); err != nil {
				t.Fatal("canonical window rejected", err)
			}
			// Invalid candidates cannot be hidden by being among the clipped oldest.
			facts[0].CLI[0].Found = -1
			if _, err := BuildWindowContent(manifest, facts); err == nil {
				t.Fatal("invalid clipped candidate was ignored")
			}
		})
	}
	if _, err := BuildWindowContent(approvedManifest(), []ReviewFacts{{Number: 2, CLI: windowRounds(1, 201)}}); err == nil {
		t.Fatal("unbounded window candidates accepted")
	}
}

func TestPublicWindowPreservesLegacyCanonicalBytesAndStrictFlags(t *testing.T) {
	manifest := approvedManifest()
	facts := []ReviewFacts{{Number: 2, CLI: windowRounds(1, 100)}}
	legacy := encodedContent(t, manifest, facts)
	window, err := BuildWindowContent(manifest, facts)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(window)
	if !bytes.Equal(raw, legacy) || bytes.Contains(raw, []byte("has_more")) {
		t.Fatal("fitting legacy canonical bytes changed")
	}
	facts[0].CLIHasMore = true
	facts[0].HostedHasMore = true
	flagged, err := BuildWindowContent(manifest, facts)
	if err != nil {
		t.Fatal(err)
	}
	flagRaw, _ := json.Marshal(flagged)
	decoded, err := DecodeContent(flagRaw)
	if err != nil || !decoded.Reviews[1].CLIHasMore || !decoded.Reviews[1].HostedHasMore {
		t.Fatal("true flags lost in strict canonical decode", err)
	}
	for _, bad := range [][]byte{
		bytes.Replace(flagRaw, []byte(`"cli_has_more":true`), []byte(`"cli_has_more":false`), 1),
		bytes.Replace(flagRaw, []byte(`"cli_has_more":true`), []byte(`"cli_has_more":null`), 1),
		bytes.Replace(flagRaw, []byte(`"hosted_has_more":true`), []byte(`"hosted_has_more":1`), 1),
		bytes.Replace(flagRaw, []byte(`"hosted_has_more":true`), []byte(`"hosted_has_more":"true"`), 1),
		bytes.Replace(flagRaw, []byte(`"cli_has_more":true`), []byte(`"CLI_has_more":true`), 1),
		bytes.Replace(flagRaw, []byte(`"cli_has_more":true`), []byte(`"cli_has_more":true,"cli_has_more":true`), 1),
		bytes.Replace(flagRaw, []byte(`"cli_has_more":true`), []byte(`"private_has_more":true`), 1),
	} {
		if _, err := DecodeContent(bad); err == nil {
			t.Fatal("malformed/noncanonical window flags accepted")
		}
	}
}

func TestPublicWindowGlobalByteTrimFairnessAndDeterminism(t *testing.T) {
	manifest := approvedManifest()
	manifest.Reviews = nil
	facts := []ReviewFacts{}
	for i := 0; i < 10; i++ {
		number := int64(i + 1)
		manifest.Reviews = append(manifest.Reviews, EnrolledReview{Number: number, Title: fmt.Sprintf("Approved PR %d", number), URL: fmt.Sprintf("https://github.com/example/project/pull/%d", number)})
		cli, hosted := windowRounds(i*1000+1, 100), windowRounds(i*1000+201, 100)
		for j := range cli {
			cli[j].Found = maxJSONInteger
			cli[j].Accepted = maxJSONInteger
			cli[j].Routed = 0
			hosted[j].Found = maxJSONInteger
			hosted[j].Accepted = maxJSONInteger
			hosted[j].Routed = 0
		}
		facts = append(facts, ReviewFacts{Number: number, Lifecycle: "merged", CLI: cli, Hosted: hosted})
	}
	if _, err := BuildContent(manifest, facts); err == nil {
		t.Fatal("fixture did not exceed strict byte budget")
	}
	before, _ := json.Marshal(struct {
		Manifest Manifest
		Facts    []ReviewFacts
	}{manifest, facts})
	got, err := BuildWindowContent(manifest, facts)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if len(raw) > MaxContentBytes || len(got.Reviews) != len(manifest.Reviews) {
		t.Fatal("global window exceeded bound or dropped approved PR")
	}
	min, max := 100, 0
	for i, review := range got.Reviews {
		if review.Number != manifest.Reviews[i].Number || review.Title != manifest.Reviews[i].Title || review.URL != manifest.Reviews[i].URL || review.Lifecycle != "merged" {
			t.Fatal("byte clipping changed approved identity/lifecycle")
		}
		for _, lane := range []struct {
			rounds []PublicRound
			more   bool
		}{{review.CLI, review.CLIHasMore}, {review.Hosted, review.HostedHasMore}} {
			if !lane.more || len(lane.rounds) >= 100 {
				t.Fatal("byte reduction omitted honest channel limit")
			}
			if len(lane.rounds) < min {
				min = len(lane.rounds)
			}
			if len(lane.rounds) > max {
				max = len(lane.rounds)
			}
		}
	}
	if max-min > 1 {
		t.Fatal("largest-array trimming starved another equal-length channel", min, max)
	}
	if _, err := DecodeContent(raw); err != nil {
		t.Fatal("byte-trimmed content not canonical", err)
	}
	after, _ := json.Marshal(struct {
		Manifest Manifest
		Facts    []ReviewFacts
	}{manifest, facts})
	if !bytes.Equal(before, after) {
		t.Fatal("byte trimming mutated inputs")
	}
	// Reversing fact and candidate arrival order leaves the selected public bytes.
	for i, j := 0, len(facts)-1; i < j; i, j = i+1, j-1 {
		facts[i], facts[j] = facts[j], facts[i]
	}
	for i := range facts {
		for _, rounds := range [][]RoundFacts{facts[i].CLI, facts[i].Hosted} {
			for j, k := 0, len(rounds)-1; j < k; j, k = j+1, k-1 {
				rounds[j], rounds[k] = rounds[k], rounds[j]
			}
		}
	}
	again, err := BuildWindowContent(manifest, facts)
	if err != nil {
		t.Fatal(err)
	}
	againRaw, _ := json.Marshal(again)
	if !bytes.Equal(raw, againRaw) {
		t.Fatal("byte window depends on input arrival order")
	}
}

// Set the editorial-only content to an exact encoded size without violating an
// individual text bound. ASCII filler makes the independently computed byte
// target exact; flag and removed-round accounting must handle a one-item array.
func editorialContentAtSize(t *testing.T, size int) Manifest {
	t.Helper()
	manifest := approvedManifest()
	manifest.ProjectMap = make([]Card, 40)
	for i := range manifest.ProjectMap {
		manifest.ProjectMap[i] = Card{Key: fmt.Sprintf("budget-%d", i), Title: "Approved"}
	}
	base := encodedContent(t, manifest, nil)
	remaining := size - len(base)
	if remaining < 0 {
		t.Fatal("editorial fixture target too small")
	}
	for i := range manifest.ProjectMap {
		n := min(remaining, 8<<10)
		manifest.ProjectMap[i].Description = strings.Repeat("x", n)
		remaining -= n
	}
	if remaining != 0 || len(encodedContent(t, manifest, nil)) != size {
		t.Fatal("editorial fixture failed exact target")
	}
	return manifest
}

func TestPublicWindowAllRoundsRemovedAccountsForFlagsOrRefusesHonestly(t *testing.T) {
	for _, channel := range []string{"cli", "hosted"} {
		t.Run(channel, func(t *testing.T) {
			field := `,"` + channel + `_has_more":true`
			manifest := editorialContentAtSize(t, MaxContentBytes-len(field))
			fact := ReviewFacts{Number: 9}
			if channel == "cli" {
				fact.CLI = windowRounds(1, 1)
			} else {
				fact.Hosted = windowRounds(1, 1)
			}
			got, err := BuildWindowContent(manifest, []ReviewFacts{fact})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(got)
			if len(raw) != MaxContentBytes || len(got.Reviews[0].CLI) != 0 || len(got.Reviews[0].Hosted) != 0 || channel == "cli" && !got.Reviews[0].CLIHasMore || channel == "hosted" && !got.Reviews[0].HostedHasMore {
				t.Fatal("empty honest lane did not retain exact byte/flag accounting")
			}
			if _, err := DecodeContent(raw); err != nil {
				t.Fatal("empty honest lane was not canonical", err)
			}
			impossible := editorialContentAtSize(t, MaxContentBytes-len(field)+1)
			if _, err := BuildWindowContent(impossible, []ReviewFacts{fact}); err == nil {
				t.Fatal("overbudget editorial+honest flag silently passed")
			}
		})
	}
}

func TestPublicWindowUnknownTimesRemainUnknownWithStableClipping(t *testing.T) {
	rounds := windowRounds(1, 200)
	for i := range rounds {
		rounds[i].ObservedAt = ""
	}
	got, err := BuildWindowContent(approvedManifest(), []ReviewFacts{{Number: 2, CLI: rounds}})
	if err != nil {
		t.Fatal(err)
	}
	if got.Reviews[1].CLI[0].Key != rounds[100].Key || !got.Reviews[1].CLIHasMore {
		t.Fatal("unknown-key clipping not deterministic")
	}
	for _, round := range got.Reviews[1].CLI {
		if round.ObservedAt != "" || round.DurationMilliseconds != nil {
			t.Fatal("window invented unknown chronology/duration")
		}
	}
	// Global duplicate eligible identity remains invalid even when oldest data
	// would be clipped out of both arrays.
	if _, err := BuildWindowContent(approvedManifest(), []ReviewFacts{{Number: 2, CLI: rounds, Hosted: rounds}}); err == nil {
		t.Fatal("clipping hid duplicate public identity")
	}
}
