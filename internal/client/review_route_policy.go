package client

// Counts must retain the same exact integer value in Go and browser clients.
const maxIncomingRoutePending int64 = (1 << 53) - 1

func validIncomingRoutePolicy(pending int64, capacity bool, reason string) bool {
	if pending < 0 || pending > maxIncomingRoutePending || capacity && pending != 0 {
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

func validSourceObligationPolicy(pending, incoming int64, capacity bool, reason string) bool {
	if pending < 0 || pending > maxIncomingRoutePending {
		return false
	}
	if pending > 0 && incoming == 0 && !capacity {
		return reason == "source_obligations_pending"
	}
	return validIncomingRoutePolicy(incoming, capacity, reason)
}
