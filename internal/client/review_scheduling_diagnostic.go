package client

func schedulingDiagnostic(reason string) *ReviewOperationDiagnostic {
	switch reason {
	case "pull_scheduling_paused":
		return &ReviewOperationDiagnostic{"New reviews are paused for this PR; this is separate from an individual operation pause. Already issued reviews can still be collected.", "Read review_pull_scheduling_read / reviews scheduling read --pr N. Resume the PR only when authorized. Existing queued work retains its identity and any separate operation pause; a terminal held operation does not resume. Do not reset rounds or grant more reviews to clear this pause."}
	case "automation_authorization_changed":
		return &ReviewOperationDiagnostic{"Automatic scheduling for this channel is off, its authorization was interrupted, or its retained authorizer no longer has the required access. This retained selection/request cannot authorize provider work.", "Read review_scheduling_read / reviews scheduling. Re-enabling scheduling does not resume an operation whose channel authorization was interrupted. Inspect this retained operation before making a new authorized request. An authorized project manager can deliberately enable scheduling for new work with its current credential and active bound worker. Keep existing issued work on its original identity; do not switch another worker's credentials or reset rounds."}
	case "automatic_intent_retained":
		return &ReviewOperationDiagnostic{"An automatic review intent already exists for these same review facts; this selection did not create another provider request.", "Inspect existing scoped operations if needed. Ticks and observation refreshes do not restart cancelled, failed or held intent. Use a PR pause to prevent new scheduling across later meaningful changes. A separate explicit request still needs normal authorization and admission."}
	}
	return nil
}
