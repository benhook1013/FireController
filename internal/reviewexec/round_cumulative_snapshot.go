package reviewexec

import (
	"context"
)

// advanceRoundReplay follows immutable completion ordinals, not timestamps or
// settings revision order. A nonnegative through is the next settings cutoff.
func advanceRoundReplay(ctx context.Context, q EvidenceQuery, c *roundRecord, through int64) error {
	if through >= 0 && through < c.Used {
		return roundUnavailable()
	}
	if through == c.Used {
		return nil
	}
	for {
		rows, err := q.QueryContext(ctx, `SELECT operation_id,evidence_id,ordinal,accepted,completed_at FROM review_round_completions WHERE project_id=? AND pull_number=? AND channel=? AND epoch=? AND ordinal>? ORDER BY ordinal LIMIT 64`, c.ProjectID, c.PullNumber, c.Channel, c.Epoch, c.Used)
		if err != nil {
			return err
		}
		type completion struct {
			operation, evidence, at string
			ordinal, accepted       int64
		}
		page := []completion{}
		for rows.Next() {
			var v completion
			if err = rows.Scan(&v.operation, &v.evidence, &v.ordinal, &v.accepted, &v.at); err != nil {
				break
			}
			page = append(page, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, v := range page {
			if through >= 0 && v.ordinal > through {
				if c.Used != through {
					return roundUnavailable()
				}
				return nil
			}
			if v.ordinal != c.Used+1 || v.ordinal > roundSafeInteger || !c.MaximumUnbounded && v.ordinal > c.Maximum || !identifier.MatchString(v.operation) || !identifier.MatchString(v.evidence) || !roundTime(v.at) {
				return roundUnavailable()
			}
			evidence, accepted, err := roundCompletionAuthority(ctx, q, c.ProjectID, v.operation)
			if err != nil {
				return err
			}
			if evidence != v.evidence || accepted != v.accepted {
				return roundUnavailable()
			}
			done := roundDone(c.RoundControl)
			c.Used++
			if c.cumulative || !done {
				if accepted == 0 {
					if c.Dry >= roundSafeInteger {
						return roundUnavailable()
					}
					c.Dry++
				} else {
					c.Dry = 0
				}
				if c.cumulative {
					c.Tapered = cumulativeTapered(c.RoundControl)
				} else if c.Used >= c.Minimum && (c.InitiallyTapered || c.Dry >= c.Taper) {
					c.Tapered = true
				}
			}
			c.UpdatedAt = v.at
			if through >= 0 && c.Used == through {
				return nil
			}
		}
		if len(page) < 64 {
			if through >= 0 && c.Used != through {
				return roundUnavailable()
			}
			return nil
		}
	}
}

func sameRoundBaseline(a, b RoundControl) bool {
	return a.ProjectID == b.ProjectID && a.PullNumber == b.PullNumber && a.Channel == b.Channel && a.Epoch == b.Epoch && a.CreatedAt == b.CreatedAt && a.Taper == b.Taper && a.InitialDry == b.InitialDry && a.InitiallyTapered == b.InitiallyTapered && a.OriginSource == b.OriginSource && a.OriginKey == b.OriginKey && a.OriginSHA256 == b.OriginSHA256
}
func validateCumulativeRoundHistory(ctx context.Context, q EvidenceQuery, project string, pull int64, channel string) error {
	current, err := readRoundRecordVersion(ctx, q, project, pull, channel, 4)
	if err != nil {
		return err
	}
	var previous roundRecord
	var revision int64
	cumulativeSeen := false
	for {
		rows, err := q.QueryContext(ctx, `SELECT revision,epoch,action,body_json,worker_id FROM review_round_changes WHERE project_id=? AND pull_number=? AND channel=? AND revision>? ORDER BY revision LIMIT 64`, project, pull, channel, revision)
		if err != nil {
			return err
		}
		type change struct {
			revision, epoch      int64
			action, body, worker string
		}
		page := []change{}
		for rows.Next() {
			var v change
			if err = rows.Scan(&v.revision, &v.epoch, &v.action, &v.body, &v.worker); err != nil {
				break
			}
			page = append(page, v)
		}
		if err == nil {
			err = rows.Err()
		}
		rows.Close()
		if err != nil {
			return err
		}
		for _, v := range page {
			c, cumulative, err := decodeRoundHistory(v.body)
			if err != nil || v.revision != revision+1 || c.ProjectID != project || c.PullNumber != pull || c.Channel != channel || c.Revision != v.revision || c.Epoch != v.epoch || v.worker != "" && !identifier.MatchString(v.worker) {
				return roundUnavailable()
			}
			record := roundRecord{RoundControl: c, cumulative: cumulative, grantRevision: previous.grantRevision}
			first := revision == 0
			newEpoch := !first && c.Epoch != previous.Epoch
			if first || newEpoch {
				if first && c.Epoch != 1 || newEpoch && (c.Epoch != previous.Epoch+1 || c.CreatedAt != previous.CreatedAt || cumulative || cumulativeSeen) {
					return roundUnavailable()
				}
				if !first {
					if err = advanceRoundReplay(ctx, q, &previous, -1); err != nil {
						return err
					}
				}
				if c.Used != 0 || c.Dry != c.InitialDry || c.Tapered != (c.InitiallyTapered && c.Minimum == 0) {
					return roundUnavailable()
				}
				if v.action == "grant" {
					if !hasRoundOrigin(c) && c.Closed || !first && hasRoundOrigin(c) {
						return roundUnavailable()
					}
					if cumulative && (hasRoundOrigin(c) || c.Closed || c.Maximum > 1000) {
						return roundUnavailable()
					}
				} else if v.action != "close" || !first || cumulative || !c.Closed || c.Minimum != 0 || c.Maximum != 1 {
					return roundUnavailable()
				}
				record.grantRevision = c.Revision
			} else {
				if !sameRoundBaseline(previous.RoundControl, c) {
					return roundUnavailable()
				}
				if err = advanceRoundReplay(ctx, q, &previous, c.Used); err != nil {
					return err
				}
				if v.action == "grant" && cumulative && !previous.cumulative {
					previous.Dry, err = cumulativeLegacyDry(ctx, q, previous)
					if err != nil {
						return err
					}
				}
				if c.Used != previous.Used || c.Dry != previous.Dry {
					return roundUnavailable()
				}
				if v.action == "close" {
					if !c.Closed || cumulative != previous.cumulative || c.Minimum != previous.Minimum || c.Maximum != previous.Maximum || c.MaximumUnbounded != previous.MaximumUnbounded || c.Tapered != previous.Tapered {
						return roundUnavailable()
					}
				} else if v.action == "grant" {
					if !cumulative || c.Closed || c.MaximumUnbounded || c.Minimum < c.Used || c.Maximum <= c.Used || c.Maximum-c.Used > 1000 || c.Minimum-c.Used > 1000 || c.Tapered != cumulativeTapered(c) {
						return roundUnavailable()
					}
					record.grantRevision = c.Revision
				} else {
					return roundUnavailable()
				}
			}
			cumulativeSeen = cumulativeSeen || cumulative
			previous = record
			revision = c.Revision
		}
		if len(page) < 64 {
			break
		}
	}
	if revision == 0 {
		return roundUnavailable()
	}
	if err = advanceRoundReplay(ctx, q, &previous, -1); err != nil {
		return err
	}
	if current.RoundControl != previous.RoundControl || current.grantRevision != previous.grantRevision || current.cumulative != previous.cumulative {
		return roundUnavailable()
	}
	return nil
}
