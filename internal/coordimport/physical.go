package coordimport

import (
	"context"
	"strings"
)

func importVersion(ctx context.Context, db SnapshotReader) (int, error) {
	var raw string
	if err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='schema:coordination_import' AND typeof(value)='text'`).Scan(&raw); err != nil {
		return 0, err
	}
	switch raw {
	case "1":
		return 1, nil
	case "2":
		return 2, nil
	default:
		return 0, conflict()
	}
}

// ValidatePhysicalSchema verifies the declared columns and key identities that
// make later import CAS and source-to-job mappings enforceable. Backup inventory
// separately owns table/column inventory and foreign-key targets/actions.
func ValidatePhysicalSchema(ctx context.Context, db SnapshotReader) error {
	version, err := importVersion(ctx, db)
	if err != nil {
		return err
	}
	var hasMessages bool
	if err = db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='coordination_import_messages' AND type='table')`).Scan(&hasMessages); err != nil {
		return err
	}
	if hasMessages != (version == 2) {
		return conflict()
	}
	type column struct {
		name, kind  string
		notNull, pk int
	}
	tables := []struct {
		name    string
		columns []column
	}{
		{"coordination_import_state", []column{
			{"project_id", "TEXT", 0, 1}, {"revision", "INTEGER", 1, 0},
		}},
		{"coordination_import_records", []column{
			{"project_id", "TEXT", 1, 1}, {"source", "TEXT", 1, 2}, {"source_key", "TEXT", 1, 3},
			{"job_id", "TEXT", 1, 0}, {"import_revision", "INTEGER", 1, 0}, {"item_sha256", "TEXT", 1, 0},
			{"source_sha256", "TEXT", 1, 0}, {"item_json", "TEXT", 1, 0}, {"created_at", "TEXT", 1, 0},
		}},
		{"coordination_import_batches", []column{
			{"project_id", "TEXT", 1, 1}, {"revision", "INTEGER", 1, 2}, {"source", "TEXT", 1, 0},
			{"source_sha256", "TEXT", 1, 0}, {"payload_sha256", "TEXT", 1, 0}, {"actor_id", "TEXT", 1, 0},
			{"worker_id", "TEXT", 1, 0}, {"created_at", "TEXT", 1, 0},
		}},
	}
	if version == 2 {
		tables = append(tables, struct {
			name    string
			columns []column
		}{"coordination_import_messages", []column{
			{"project_id", "TEXT", 1, 1}, {"source", "TEXT", 1, 2}, {"source_key", "TEXT", 1, 3},
			{"message_id", "TEXT", 1, 0}, {"conversation_id", "TEXT", 1, 0}, {"reply_message_id", "TEXT", 1, 0},
			{"import_revision", "INTEGER", 1, 0}, {"item_sha256", "TEXT", 1, 0}, {"source_sha256", "TEXT", 1, 0}, {"item_json", "TEXT", 1, 0}, {"created_at", "TEXT", 1, 0},
		}})
	}
	for _, table := range tables {
		rows, err := db.QueryContext(ctx, `SELECT cid,name,type,"notnull",pk FROM pragma_table_info(?) ORDER BY cid`, table.name)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var cid int
			var got column
			if err = rows.Scan(&cid, &got.name, &got.kind, &got.notNull, &got.pk); err != nil {
				break
			}
			if count >= len(table.columns) || cid != count || got != table.columns[count] {
				err = conflict()
				break
			}
			count++
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		if count != len(table.columns) {
			return conflict()
		}
		keys := [][]string{}
		for _, c := range table.columns {
			if c.pk > 0 {
				if len(keys) == 0 {
					keys = append(keys, []string{})
				}
				keys[0] = append(keys[0], c.name)
			}
		}
		if table.name == "coordination_import_records" {
			keys = append(keys, []string{"project_id", "job_id"})
		}
		if table.name == "coordination_import_messages" {
			keys = append(keys, []string{"project_id", "message_id"})
		}
		for keyIndex, names := range keys {
			query := `SELECT EXISTS(SELECT 1 FROM pragma_index_list(?) i WHERE i."unique"=1 AND i.partial=0`
			args := []any{table.name}
			if keyIndex == 0 {
				query += ` AND i.origin='pk'`
			}
			query += ` AND (SELECT count(*) FROM pragma_index_xinfo(i.name) WHERE key=1)=?`
			args = append(args, len(names))
			for position, name := range names {
				cid := -1
				for i, c := range table.columns {
					if c.name == name {
						cid = i
						break
					}
				}
				query += ` AND EXISTS(SELECT 1 FROM pragma_index_xinfo(i.name) WHERE key=1 AND seqno=? AND name=? AND cid=? AND coll='BINARY' AND "desc"=0)`
				args = append(args, position, name, cid)
			}
			query += `)`
			var valid bool
			if err := db.QueryRowContext(ctx, query, args...).Scan(&valid); err != nil {
				return err
			}
			if !valid {
				return conflict()
			}
		}
		// SQLite affinity does not prohibit BLOB keys in TEXT columns. Refuse
		// noncanonical storage classes before keyset paging or text selectors:
		// scanning a BLOB into a Go string would hide that identity difference.
		checks := make([]string, len(table.columns))
		args := make([]any, 0, len(table.columns))
		for i, c := range table.columns {
			checks[i] = `typeof("` + c.name + `")<>?`
			args = append(args, strings.ToLower(c.kind))
		}
		var invalidType bool
		if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM "`+table.name+`" WHERE `+strings.Join(checks, " OR ")+`)`, args...).Scan(&invalidType); err != nil {
			return err
		}
		if invalidType {
			return conflict()
		}
	}
	return ctx.Err()
}
