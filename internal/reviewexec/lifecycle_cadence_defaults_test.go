package reviewexec

import (
	"testing"
	"time"
)

func TestLifecycleCadenceDefaults(t *testing.T) {
	plain, err := lifecycleOptions(nil)
	if err != nil {
		t.Fatal(err)
	}
	if plain.OpenInterval != time.Minute || plain.CIInterval != 5*time.Minute || plain.ClosedInterval != time.Hour || plain.MergedInterval != 30*24*time.Hour {
		t.Fatalf("unexpected defaults %+v", plain)
	}
	hooked, err := lifecycleOptions([]LifecycleSyncOptions{{WebhookHints: true}})
	if err != nil {
		t.Fatal(err)
	}
	if hooked.OpenInterval != 10*time.Minute || hooked.CIInterval != 15*time.Minute || hooked.ClosedInterval != time.Hour {
		t.Fatalf("webhook fallback not applied %+v", hooked)
	}
	// Explicit intervals still win over the webhook fallback.
	explicit, err := lifecycleOptions([]LifecycleSyncOptions{{WebhookHints: true, OpenInterval: time.Second, CIInterval: 2 * time.Second}})
	if err != nil || explicit.OpenInterval != time.Second || explicit.CIInterval != 2*time.Second {
		t.Fatalf("explicit intervals overridden %+v %v", explicit, err)
	}
}
