package reviewexec

import "testing"

func TestLifecycleQualificationOptionsAreExclusiveAndDefaultOff(t *testing.T) {
	defaults, err := lifecycleOptions(nil)
	if err != nil || defaults.QualifyCIBatching || defaults.BatchCIWitnesses {
		t.Fatal(defaults, err)
	}
	_, err = lifecycleOptions([]LifecycleSyncOptions{{QualifyCIBatching: true, BatchCIWitnesses: true}})
	if err == nil {
		t.Fatal("simultaneous qualification and activation accepted")
	}
	selected, err := lifecycleOptions([]LifecycleSyncOptions{{QualifyCIBatching: true}})
	if err != nil || !selected.QualifyCIBatching || selected.BatchCIWitnesses {
		t.Fatal(selected, err)
	}
}
