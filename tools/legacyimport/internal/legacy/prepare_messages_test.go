package legacy

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
)

func messageSelection() MessageSelection {
	revision := int64(0)
	return MessageSelection{Version: 1, RepositoryID: 7, ExpectedRevision: &revision, Source: "selected-history", MessageIDs: []string{"z-reply", "b-unknown", "a-root"}, Workers: map[string]string{"original-sender": "worker_sender", "original-recipient": "worker_recipient"}, UnknownAuthorWorkerID: "worker_sender", AssociationPolicy: "archive"}
}
func selectedMessageFixture(t *testing.T, mutate func(*sql.DB)) string {
	return snapshotFixture(t, func(db *sql.DB) {
		execFixture(t, db, `INSERT INTO inbox_messages VALUES('a-root','original-recipient','Original root 雪 <inert>','original-sender','archived-job',7,NULL,'2024-01-01T00:00:00.123456789Z',NULL,NULL)`)
		execFixture(t, db, `INSERT INTO inbox_messages VALUES('b-unknown','original-recipient','Original unknown author',NULL,NULL,NULL,NULL,'2024-01-01T02:00:00.123456789+02:00','2024-01-01T03:00:00+02:00',NULL)`)
		execFixture(t, db, `INSERT INTO inbox_messages VALUES('z-reply','original-sender','  Original reply\nexact  ','original-recipient',NULL,7,'a-root','2024-01-02T03:04:05Z','2024-01-02T03:05:05+00:00','2024-01-02T04:06:05+01:00')`)
		execFixture(t, db, `INSERT INTO inbox_messages VALUES('unselected','original-recipient','Never include this','original-sender',NULL,NULL,NULL,'2024-01-03T00:00:00Z',NULL,NULL)`)
		if mutate != nil {
			mutate(db)
		}
	})
}
func TestPrepareMessagesPreservesExplicitFactsAndCreationOrder(t *testing.T) {
	path := selectedMessageFixture(t, nil)
	before := sourceBytes(t, path)
	s := messageSelection()
	ids := append([]string(nil), s.MessageIDs...)
	batch, summary, err := PrepareMessages(t.Context(), path, Limits{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Kind != "prepared_message_import" || summary.SourceSHA256 != batch.SourceSHA256 || summary.Messages != 3 || summary.Replies != 1 || summary.ArchivedAssociations != 3 || summary.UnknownAuthors != 1 || batch.Version != 1 || batch.RepositoryID != 7 || batch.ExpectedRevision == nil || *batch.ExpectedRevision != 0 || len(batch.Jobs) != 0 || batch.Jobs == nil {
		t.Fatal("incorrect selected scope/summary")
	}
	if !reflect.DeepEqual(ids, s.MessageIDs) {
		t.Fatal("selection mutated")
	}
	a, b, c := batch.Messages[0], batch.Messages[1], batch.Messages[2]
	if a.SourceKey != "a-root" || b.SourceKey != "b-unknown" || c.SourceKey != "z-reply" {
		t.Fatal("not ordered by original creation instant/stable source ID")
	}
	if a.Body != "Original root 雪 <inert>" || a.CreatedAt != "2024-01-01T00:00:00.123456789Z" || a.SeenAt != "" || a.AcknowledgedAt != "" || a.SenderWorkerID != "worker_sender" || a.RecipientWorkerID != "worker_recipient" {
		t.Fatal("root originals changed")
	}
	if b.SenderWorkerID != "worker_sender" || b.CreatedAt != "2024-01-01T02:00:00.123456789+02:00" || b.SeenAt != "2024-01-01T03:00:00+02:00" || b.AcknowledgedAt != "" {
		t.Fatal("explicit unknown-author/handling facts changed")
	}
	if c.ReplyToSourceKey != "a-root" || c.Body != "  Original reply\\nexact  " || c.SenderWorkerID != "worker_recipient" || c.SeenAt != "2024-01-02T03:05:05+00:00" || c.AcknowledgedAt != "2024-01-02T04:06:05+01:00" {
		t.Fatal("reply originals changed")
	}
	checkConserved(t, path, before)
}
func TestPrepareMessagesExplicitRetainedParentBoundary(t *testing.T) {
	path := selectedMessageFixture(t, func(db *sql.DB) {
		execFixture(t, db, `INSERT INTO inbox_messages VALUES('outside','original-recipient','Retained ancestor','unmapped-outside-author',NULL,NULL,NULL,'2023-01-01T00:00:00Z',NULL,NULL)`)
		execFixture(t, db, `UPDATE inbox_messages SET reply_to='outside' WHERE id='a-root'`)
	})
	s := messageSelection()
	s.MessageIDs = []string{"z-reply"}
	s.RetainedParentIDs = []string{"a-root"}
	s.UnknownAuthorWorkerID = ""
	before := sourceBytes(t, path)
	b, summary, err := PrepareMessages(t.Context(), path, Limits{}, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(b.Messages) != 1 || b.Messages[0].ReplyToSourceKey != "a-root" || summary.Replies != 1 || summary.ArchivedAssociations != 1 || summary.UnknownAuthors != 0 {
		t.Fatal("retained parent was imported or scope changed")
	}
	checkConserved(t, path, before)
	s.RetainedParentIDs = nil
	_, _, err = PrepareMessages(t.Context(), path, Limits{}, s)
	inspectError(t, err, "snapshot_conversion_blocked")
}
func TestPrepareMessagesSelectionRefusals(t *testing.T) {
	path := selectedMessageFixture(t, nil)
	before := sourceBytes(t, path)
	cases := map[string]func(*MessageSelection){
		"missing-author-mapping":    func(s *MessageSelection) { delete(s.Workers, "original-sender") },
		"missing-recipient-mapping": func(s *MessageSelection) { delete(s.Workers, "original-recipient") },
		"unknown-author-unmapped":   func(s *MessageSelection) { s.UnknownAuthorWorkerID = "" },
		"empty-selection":           func(s *MessageSelection) { s.MessageIDs = nil },
		"missing-selected":          func(s *MessageSelection) { s.MessageIDs = []string{"missing"} },
		"duplicate-selected":        func(s *MessageSelection) { s.MessageIDs = append(s.MessageIDs, s.MessageIDs[0]) },
		"selected-retained-overlap": func(s *MessageSelection) { s.RetainedParentIDs = []string{"a-root"} },
		"missing-retained":          func(s *MessageSelection) { s.RetainedParentIDs = []string{"missing"} },
		"duplicate-retained": func(s *MessageSelection) {
			s.MessageIDs = []string{"z-reply"}
			s.RetainedParentIDs = []string{"a-root", "a-root"}
		},
		"unselected-parent":      func(s *MessageSelection) { s.MessageIDs = []string{"z-reply"} },
		"no-archive-policy":      func(s *MessageSelection) { s.AssociationPolicy = "" },
		"coerce-associations":    func(s *MessageSelection) { s.AssociationPolicy = "convert" },
		"invalid-native-worker":  func(s *MessageSelection) { s.Workers["unused"] = "padded worker" },
		"invalid-unknown-worker": func(s *MessageSelection) { s.UnknownAuthorWorkerID = "invalid worker" },
		"source":                 func(s *MessageSelection) { s.Source = " padded " },
		"no-cas":                 func(s *MessageSelection) { s.ExpectedRevision = nil },
		"too-many":               func(s *MessageSelection) { s.MessageIDs = make([]string, 101) },
		"too-many-parents":       func(s *MessageSelection) { s.RetainedParentIDs = make([]string, 101) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := messageSelection()
			change(&s)
			_, _, err := PrepareMessages(t.Context(), path, Limits{}, s)
			inspectError(t, err, "snapshot_conversion_blocked", "Original root", "original-sender")
			checkConserved(t, path, before)
		})
	}
}
func TestPrepareMessagesRefusesCoercionAmbiguityGraphAndNativeLimits(t *testing.T) {
	cases := map[string]string{
		"blob-body":                `UPDATE inbox_messages SET body=CAST(body AS BLOB) WHERE id='a-root'`,
		"blob-author":              `UPDATE inbox_messages SET author=CAST(author AS BLOB) WHERE id='a-root'`,
		"blob-id":                  `UPDATE inbox_messages SET id=CAST(id AS BLOB) WHERE id='a-root'`,
		"blob-time":                `UPDATE inbox_messages SET seen_at=CAST(seen_at AS BLOB) WHERE id='z-reply'`,
		"empty-author":             `UPDATE inbox_messages SET author='' WHERE id='a-root'`,
		"empty-seen-is-not-absent": `UPDATE inbox_messages SET seen_at='' WHERE id='b-unknown'`,
		"timing-regression":        `UPDATE inbox_messages SET seen_at='2020-01-01T00:00:00Z' WHERE id='b-unknown'`,
		"wrong-participants":       `UPDATE inbox_messages SET author='third' WHERE id='z-reply'`,
		"cycle":                    `UPDATE inbox_messages SET reply_to='z-reply' WHERE id='a-root'`,
		"invalid-utf8":             `UPDATE inbox_messages SET body=CAST(x'ff' AS TEXT) WHERE id='a-root'`,
		"duplicate":                `ALTER TABLE inbox_messages RENAME TO saved_messages; CREATE TABLE inbox_messages AS SELECT * FROM saved_messages; INSERT INTO inbox_messages SELECT * FROM saved_messages WHERE id='a-root'; DROP TABLE saved_messages`,
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			path := selectedMessageFixture(t, func(db *sql.DB) { execFixture(t, db, query) })
			before := sourceBytes(t, path)
			s := messageSelection()
			s.Workers["third"] = "worker_third"
			_, _, err := PrepareMessages(t.Context(), path, Limits{}, s)
			inspectError(t, err, "snapshot_conversion_blocked")
			checkConserved(t, path, before)
		})
	}
	for name, size := range map[string]int{"individual-body": 16385, "eventual-wrapper": 16000} {
		t.Run(name, func(t *testing.T) {
			path := selectedMessageFixture(t, func(db *sql.DB) {
				execFixture(t, db, `UPDATE inbox_messages SET body=? WHERE id='a-root'`, strings.Repeat("x", size))
				if name == "eventual-wrapper" {
					for i := 0; i < 20; i++ {
						execFixture(t, db, `INSERT INTO inbox_messages VALUES(?,'original-recipient',?,'original-sender',NULL,NULL,NULL,'2024-01-01T00:00:00Z',NULL,NULL)`, string(rune('A'+i)), strings.Repeat("x", size))
					}
				}
			})
			s := messageSelection()
			if name == "eventual-wrapper" {
				s.MessageIDs = nil
				for i := 0; i < 20; i++ {
					s.MessageIDs = append(s.MessageIDs, string(rune('A'+i)))
				}
			}
			before := sourceBytes(t, path)
			_, _, err := PrepareMessages(t.Context(), path, Limits{}, s)
			inspectError(t, err, "snapshot_conversion_blocked")
			checkConserved(t, path, before)
		})
	}
}

func TestPrepareMessagesCreationOrderUsesTimestampInstants(t *testing.T) {
	path := selectedMessageFixture(t, func(db *sql.DB) {
		execFixture(t, db, `UPDATE inbox_messages SET created_at='2024-01-01T02:00:00+13:00',seen_at=NULL WHERE id='b-unknown'`)
	})
	b, _, err := PrepareMessages(t.Context(), path, Limits{}, messageSelection())
	if err != nil {
		t.Fatal(err)
	}
	if b.Messages[0].SourceKey != "b-unknown" || b.Messages[1].SourceKey != "a-root" {
		t.Fatal("source order used timestamp spelling instead of creation instant")
	}
}
