# Project review activity

Read completed Hosted and CLI reviews across the project in one provider-free call. This includes retained results for queued, merged and closed PRs; workers do not need to enumerate PRs or aggregate run pages.

```sh
fc reviews activity --hours 24 --timezone Pacific/Auckland
fc reviews activity --since 2026-10-01T00:00:00Z --until 2026-10-02T00:00:00Z --timezone UTC --pr 42
```

The MCP equivalent is `review_activity_read`. The HTTP endpoint is `GET /projects/{project_id}/reviews/activity`, with optional `hours`, `since`, `until`, `timezone` and `pull_number` query parameters. These are ordinary authenticated project reads and require no review operation or provider credentials.

## Window and output

The default window is the preceding 24 hours in UTC. Select an integer `hours` from 1 through 168, or supply both RFC3339 `since` and `until` timestamps for a window of at most seven days. These forms cannot be combined. `timezone` accepts an IANA name for bucket labels; it does not change which instants belong to the window. An optional PR filter narrows the same read.

The response contains Hosted and CLI totals, averages per elapsed hour, chronological hourly buckets, separate ended-attempt diagnostics and explicit coverage limits. Buckets align to UTC hours and clip to the selected window. Local labels include their UTC offset, so repeated daylight-saving hours remain distinguishable. A partial bucket is marked explicitly; averages use the full selected elapsed duration rather than the number of bucket rows.

## Counting and coverage

Only complete results with a completed outcome count, including genuine zero-finding reviews and completed results awaiting adjudication. The original review finish time determines the bucket. Later import, observation, adjudication or fix activity does not move the review into a new hour. Current retained identities deduplicate repeated snapshots and reconciled native/observer aliases; unrelated imported producer identities are not guessed equivalent from their text or timestamps.

Failed, held, interrupted and unproven ended attempts appear separately and do not increase review totals. Their original finish time is required. Failed result captures are not counted again as attempt diagnostics. Reviews outside the Hosted and CLI channels are excluded.

Coverage describes the current retained history, not an exhaustive provider inventory, review credit or available quota. Undated result and attempt counts cover the selected project's retained history, narrowed by the optional PR filter; they cannot be assigned to the requested time window. Completed but incomplete results with a finish time inside the window are excluded and reported separately. Historical review activity does not establish coverage of later code.

Reads use the existing storage deadline and refuse oversized windows of candidate records with `review_activity_limit`; a storage deadline may instead return `overloaded`. Narrow the window or select a PR. Neither response contains partial totals. The read fetches no GitHub/provider data, starts no reviews and writes no events, receipts or accounting records.
