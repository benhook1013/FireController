package reviewexec

import "testing"

func TestAutomaticDependencyReadSharesParentsOnlyWithinSnapshot(t *testing.T) {
	frame := frontFrame{prefix: []frontPull{
		{Number: 5, ParentPullNumber: 4}, {Number: 4, ParentPullNumber: 3},
		{Number: 3, ParentPullNumber: 2}, {Number: 2, ParentPullNumber: 1},
		{Number: 1, ParentPullNumber: 9},
	}}
	var loads []int64
	newRead := func(revision int) automaticDependencyRead {
		read := newAutomaticDependencyRead(t.Context(), nil, "test-project", "hosted", frame, true)
		read.load = func(p frontPull) ([]any, error) {
			loads = append(loads, p.Number)
			return []any{p.Number, revision}, nil
		}
		return read
	}
	expect := func(read automaticDependencyRead, pull int64, want string) {
		t.Helper()
		_, got, err := read.formatsForPull(pull)
		if err != nil || encoded(got) != want {
			t.Fatalf("pull %d: got %s, err=%v, want %s", pull, encoded(got), err, want)
		}
	}
	reader := newRead(1)
	expect(reader, 4, `[[3,1],[2,1],[1,1],[9,"source_prefix_incomplete"]]`)
	expect(reader, 5, `[[4,1],[3,1],[2,1],[1,1],[9,"source_prefix_incomplete"]]`)
	if encoded(loads) != "[3,2,1,4]" {
		t.Fatalf("reloaded shared ancestors: %v", loads)
	}

	loads = nil
	expect(newRead(2), 5, `[[4,2],[3,2],[2,2],[1,2],[9,"source_prefix_incomplete"]]`)
	if encoded(loads) != "[4,3,2,1]" {
		t.Fatalf("new snapshot reused old facts: %v", loads)
	}
}
