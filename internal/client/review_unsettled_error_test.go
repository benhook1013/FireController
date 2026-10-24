package client

import (
	"strings"
	"testing"
)

func TestUnsettledReviewResultExplainsOriginalRecoveryWithoutLeakingProviderText(t *testing.T) {
	c := &Client{token: "private-token-sentinel", credentialFile: "/private/credential"}
	e, ok := c.serviceError([]byte(`{"error":{"code":"review_result_unsettled","message":"PRIVATE_PROVIDER_TEXT"},"request_id":"request-unsettled"}`), 409)
	if !ok || e.Code != "review_result_unsettled" || !strings.Contains(e.Action, "original review operation") || !strings.Contains(e.Action, "Do not rerun") || strings.Contains(e.Message, "PRIVATE_PROVIDER_TEXT") || strings.Contains(e.Action, "Check the selected instance") {
		t.Fatal(e, ok)
	}
}
