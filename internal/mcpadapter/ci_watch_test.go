package mcpadapter

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCIWatchMCPArgumentsAndQuietBudget(t *testing.T) {
	target := `{"pull_number":2,"head_sha":"` + strings.Repeat("a", 40) + `"}`
	for _, seconds := range []int{0, 300, 900} {
		raw, _ := json.Marshal(map[string]any{"targets": []json.RawMessage{json.RawMessage(target)}, "wait_seconds": seconds})
		args, err := parseArguments("wait_for_ci", raw)
		if err != nil || args.reviewSourceWait != seconds || len(args.ciWatchTargets) != 1 || args.key != "" || toolCallBudget("wait_for_ci", args) != max(15*time.Second, time.Duration(seconds+15)*time.Second) {
			t.Fatal("CI quiet read did not retain its preference", args, err)
		}
	}
	args, err := parseArguments("wait_for_ci", json.RawMessage(`{"targets":[`+target+`]}`))
	if err != nil || args.reviewSourceWait != 300 {
		t.Fatal("CI default changed", args, err)
	}
	for _, raw := range []string{`{}`, `{"targets":null}`, `{"targets":[]}`, `{"targets":[` + target + `,` + target + `]}`, `{"targets":[` + target + `],"wait_seconds":null}`, `{"targets":[` + target + `],"wait_seconds":901}`, `{"targets":[` + target + `],"idempotency_key":"forbidden"}`, `{"targets":[` + strings.TrimSuffix(target, "}") + `,"base_sha":null}]}`, `{"targets":[` + strings.TrimSuffix(target, "}") + `,"base_sha":""}]}`, `{"targets":[` + strings.Replace(target, "head_sha", "HeadSHA", 1) + `]}`} {
		if _, err := parseArguments("wait_for_ci", json.RawMessage(raw)); err == nil {
			t.Fatal("unsafe CI watch arguments accepted", raw)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	fit := fitReviewWait(ctx, "wait_for_ci", args)
	if fit.reviewSourceWait != 0 || !reflect.DeepEqual(fit.ciWatchTargets, args.ciWatchTargets) {
		t.Fatal("setup fitting changed targets or kept an impossible wait")
	}
	d := ciWatchDefinition()
	wait := d.schema["properties"].(map[string]any)["wait_seconds"].(map[string]any)
	if wait["maximum"] != 900 || wait["default"] != 300 {
		t.Fatal("CI discovery drifted from validation", wait)
	}
}

func TestCIWatchMCPZeroWaitDiscoveryDoesNotPromiseCachedResults(t *testing.T) {
	definition := ciWatchDefinition()
	wait := definition.schema["properties"].(map[string]any)["wait_seconds"].(map[string]any)
	description := wait["description"].(string)
	for _, phrase := range []string{"does not wait for collection", "or reuse cached observations", "normally returns not_observed", "fresh observation completed during this call", "positive wait"} {
		if !strings.Contains(description, phrase) {
			t.Fatal("zero-wait discovery lost its fresh-only boundary", description)
		}
	}
	if strings.Contains(description, "available results immediately") {
		t.Fatal("zero wait still promises cached results", description)
	}
}
