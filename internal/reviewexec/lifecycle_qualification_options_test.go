package reviewexec

import "testing"

func TestLifecycleQualificationOptionsAreExclusiveAndDefaultOff(t *testing.T) {
	defaults, err := lifecycleOptions(nil)
	if err != nil || defaults.QualifyCIBatching || defaults.BatchCIWitnesses || defaults.QualifyMetadataPair || defaults.BatchMetadata {
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

func TestLifecycleMetadataQualificationExclusiveAndIndependent(t *testing.T) {
	if _, err := lifecycleOptions([]LifecycleSyncOptions{{QualifyMetadataPair: true, BatchMetadata: true}}); err == nil {
		t.Fatal("metadata qualification and activation accepted together")
	}
	selected, err := lifecycleOptions([]LifecycleSyncOptions{{QualifyMetadataPair: true, BatchCIWitnesses: true}})
	if err != nil || !selected.QualifyMetadataPair || selected.BatchMetadata || !selected.BatchCIWitnesses {
		t.Fatal("unrelated CI mode changed", selected, err)
	}
}
