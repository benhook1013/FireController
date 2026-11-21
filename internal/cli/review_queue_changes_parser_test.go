package cli

import (
	"reflect"
	"strings"
	"testing"
)

func TestQueueRestoreReorderDispatchAndHelpExamples(t *testing.T) {
	for _, action := range []string{"add", "move", "remove", "restore", "reorder"} {
		help, ok := operationHelp("reviews queue-" + action)
		if !ok {
			t.Fatal("missing operation help", action)
		}
		call, ok := parseInvocation(strings.Fields(strings.TrimPrefix(help.Example, "<bound launcher> ")))
		if !ok || call.reviewQueueChange == nil || call.reviewQueueChange.action != action || call.reviewQueueChange.key == "" {
			t.Fatal("example did not dispatch its own action", action, help.Example, call, ok)
		}
	}
	for _, destination := range [][]string{nil, {"--before", "17"}, {"--after=17"}, {"--first"}, {"--last"}} {
		args := append([]string{"reviews", "queue-restore", "42", "--expect-config=3", "--key=restore-42"}, destination...)
		call, ok := parseInvocation(args)
		if !ok || call.reviewQueueChange == nil || call.reviewQueueChange.action != "restore" || call.reviewQueueChange.input.PullNumber != 42 || call.reviewQueueChange.input.ExpectedConfigurationRevision != 3 {
			t.Fatal(args, call, ok)
		}
	}
	call, ok := parseInvocation([]string{"reviews", "queue-reorder", "--order=42,17", "--expect-config=3", "--key=reorder-42-17"})
	if !ok || call.reviewQueueChange == nil || call.reviewQueueChange.action != "reorder" || call.reviewQueueChange.input.PullNumber != 0 || !reflect.DeepEqual(call.reviewQueueChange.input.Order, []int64{42, 17}) {
		t.Fatal(call, ok)
	}
	parent, ok := operationHelp("reviews help")
	if !ok {
		t.Fatal("missing reviews help")
	}
	for _, action := range []string{"restore", "reorder"} {
		if !strings.Contains(strings.Join(parent.Optional, " "), "reviews queue-"+action+" --help") {
			t.Fatal("reviews help omitted queue action", action)
		}
	}
}
