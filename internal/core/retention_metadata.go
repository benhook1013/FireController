package core

// ReviewHistoryFloorPrefix is the durable visibility fence for archived
// snapshots. Older physical snapshots may survive for proof references.
const ReviewHistoryFloorPrefix = "retention:review_history_floor:"
