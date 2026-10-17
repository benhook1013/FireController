package legacy

import _ "embed"

// Synthetic source schema facts shared by reader and actual-command tests.
// No legacy runtime or private database is executed or copied.
//
//go:embed testdata/snapshot-schema.sql
var fixtureSchema string
