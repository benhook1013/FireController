// Package legacy inspects explicitly supplied, completed standalone exports of
// the legacy controller. It is not a live-database reader, converter or importer.
package legacy

// Limits bound one inspection. Zero fields select defaults; negative values
// are invalid. Raising a limit requires an explicit caller choice.
type Limits struct {
	MaxDatabaseBytes  int64
	MaxRowsPerTable   int64
	MaxTotalRows      int64
	MaxCellBytes      int64
	MaxTotalCellBytes int64
}

func DefaultLimits() Limits { return Limits{512 << 20, 100000, 500000, 8 << 20, 256 << 20} }

// Error contains only fixed public diagnostics, never source paths or SQL errors.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }
func failure(code string) *Error {
	messages := map[string]string{
		"limits_invalid":              "Inspection limits must be positive bounded integers or zero defaults.",
		"snapshot_invalid":            "Supply an absolute path to a private regular standalone snapshot, without a symlink.",
		"snapshot_unsealed":           "The snapshot has adjacent transaction files or their absence cannot be verified; supply a completed standalone export.",
		"snapshot_too_large":          "The selected snapshot exceeds an explicit inspection size, row or cell limit; nothing was truncated.",
		"snapshot_schema_unsupported": "The snapshot does not have a supported legacy controller table and version layout.",
		"snapshot_unavailable":        "The selected snapshot could not be inspected read-only.",
		"inspection_interrupted":      "Snapshot inspection was canceled or its deadline expired.",
		"source_changed":              "Snapshot identity or bytes changed during inspection; use a completed standalone export.",
	}
	return &Error{code, messages[code]}
}

type SchemaVersions struct {
	SQLite             int64 `json:"sqlite"`
	Controller         int64 `json:"controller"`
	MinimumWriterBuild int64 `json:"minimum_writer_build"`
	Jobs               int64 `json:"jobs"`
	Inbox              int64 `json:"inbox"`
	Map                int64 `json:"map"`
	Reviews            int64 `json:"reviews"`
}
type BriefStats struct {
	Count            int64 `json:"count"`
	MaxUTF8Bytes     int64 `json:"max_utf8_bytes"`
	NativeLimitBytes int64 `json:"native_limit_bytes"`
	OverNativeLimit  int64 `json:"over_native_limit"`
	InvalidText      int64 `json:"invalid_text"`
}
type NoteStats struct {
	InstructionNotes         int64 `json:"instruction_notes"`
	UnsupportedKinds         int64 `json:"unsupported_kinds"`
	InstructionRevisions     int64 `json:"instruction_revisions"`
	UnsupportedRevisionKinds int64 `json:"unsupported_revision_kinds"`
	InvalidRevisions         int64 `json:"invalid_revisions"`
}
type MessageStats struct {
	HandlingTimestampsAvailable bool  `json:"handling_timestamps_available"`
	Replies                     int64 `json:"replies"`
	OrphanReplies               int64 `json:"orphan_replies"`
	SelfReplies                 int64 `json:"self_replies"`
	CyclicReplyMessages         int64 `json:"cyclic_reply_messages"`
	Seen                        int64 `json:"seen"`
	Acknowledged                int64 `json:"acknowledged"`
	AckWithoutSeen              int64 `json:"ack_without_seen"`
	UnknownAuthors              int64 `json:"unknown_authors"`
	InvalidFields               int64 `json:"invalid_fields"`
	InvalidTimestamps           int64 `json:"invalid_timestamps"`
}
type Diagnostic struct {
	Code  string `json:"code"`
	Count int64  `json:"count"`
}
type Inspection struct {
	Kind        string           `json:"kind"`
	SHA256      string           `json:"sha256"`
	Bytes       int64            `json:"bytes"`
	Versions    SchemaVersions   `json:"versions"`
	Counts      map[string]int64 `json:"counts"`
	Briefs      BriefStats       `json:"briefs"`
	Notes       NoteStats        `json:"notes"`
	Messages    MessageStats     `json:"messages"`
	Diagnostics []Diagnostic     `json:"diagnostics"`
}
