package client

func validIncomingRoutePolicy(pending int64, capacity bool, reason string) bool {
	if pending < 0 || pending > 100 || capacity && pending != 0 {
		return false
	}
	if capacity {
		return reason == "routed_findings_capacity"
	}
	if pending > 0 {
		return reason == "routed_findings_pending"
	}
	return reason == ""
}
