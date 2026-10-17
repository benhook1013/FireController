# Hosting and HTTPS

## Supported boundary

One FireController service serves the API, live event transport and embedded browser. Local and remote installations use the same application and project permissions. Hosting remotely does not make private project data public. The [public project website](public-site.md) implements an explicit opt-in projection; private hosting or a service upgrade alone does not publish project data.

The current daemon accepts only loopback listeners. Remote deployment places a TLS reverse proxy in front of that listener. The Go CLI and local stdio MCP adapter can use an HTTPS service URL in their private profiles; a remote controller does not require a remotely exposed MCP endpoint. See [client binding](agent-client.md) and [MCP configuration](mcp.md#configure-one-agent-binding).

## A single authoritative installation

Run one active service replica with SQLite on the server's local filesystem. One instance can serve multiple projects; each client retains its explicit instance, project, worker and credential binding. Clients use the API rather than opening the database or requiring a server checkout.

An installation may run on one personal computer or an always-on server. Keeping a local development installation does not make it a synchronized replica of a remote instance. Remote clients depend on network availability and latency; local benchmark results do not establish remote response times.

## Certificates belong to deployment infrastructure

Terminate HTTPS in a reverse proxy such as Caddy or an existing Kubernetes ingress. The infrastructure obtains and renews its certificate through its configured certificate manager. FireController does not implement ACME or require a particular hosting provider.

For a public hostname, configure DNS for the intended server and provision a publicly trusted certificate through that infrastructure. Browsers and agents verify the hostname and trust chain normally. A private LAN installation can instead use a private CA, but each accessing device must trust it explicitly. Never disable certificate verification as an installation step.

Keep project authentication enabled behind the proxy. Preserve the public Host header and forward authorization, instance, worker and browser-session selector headers unchanged; do not put credentials in URLs or proxy access logs. Both local and remote access follow the same server authorization checks. Forward Set-Cookie responses and browser cookies without exposing them in logs; the [browser session API](local-api.md#browser-sessions) uses Secure cookies on public hostnames even when the application receives loopback HTTP from its TLS proxy.

For a standalone host where Caddy owns the HTTP/HTTPS ports, the proxy configuration can be:

```caddyfile
controller.example.com {
    reverse_proxy 127.0.0.1:8080 {
        flush_interval -1
    }
}
```

Replace the example hostname with the operator's configured hostname. Existing ingress installations can provide the equivalent routing and certificate lifecycle. In Kubernetes, a proxy in the same pod can reach the daemon's loopback listener while exposing only the proxy through a ClusterIP service. The application data volume belongs only to the controller container. Keep the deployment separate from disposable preview namespaces and cleanup.

## Persistence, streams and updates

Use an owner-private persistent data directory and one controller writer. A deployment update must stop the predecessor before starting its replacement; a one-replica rolling deployment can still overlap writers, so use a replacement strategy that prevents that overlap. Readiness checks use `/readyz`; provider outages do not make local coordination unready.

A container with a read-only root filesystem also needs writable temporary storage for SQLite operations, including startup migrations. Mount a bounded temporary volume at `/tmp`; keep it separate from persistent data and proxy scratch files. The verified container configuration uses a controller-only 64 MiB memory-backed temporary volume while retaining the read-only root filesystem and non-root identity. A health check on an empty database does not prove that migrations of retained data can run under those filesystem constraints.

The implemented browser feed uses SSE over HTTPS. Disable response buffering and allow idle intervals longer than its fifteen-second transport keepalive at every proxy. Verify actual frame delivery through the deployed route; a successful page load alone does not prove streaming. WebSocket transport and remote MCP endpoints remain separate, unimplemented capabilities.

Before relocating an instance, stop its existing writer, capture a consistent database snapshot and required private files, and verify integrity, identities and retained records at the destination. Switch clients only after destination checks pass and keep the predecessor stopped. A stale predecessor is not a rollback database after new writes reach the destination. The [architecture recovery contract](architecture.md#storage-operations-and-growth) owns disaster restore, credential rotation and fencing; a planned relocation is distinct from disaster restore.

Local [operator backup, inspection, inactive restore and managed scheduling](backups.md) are implemented. Remote/browser backup controls and automatic off-host transfer remain unimplemented. A migration backup does not establish an installed recurring backup policy or off-host durability.

## Executed deployment proof

The original single-server deployment used source `648c363a34ea52ffa36573260802602f2adf8745` and packaged `fcd` SHA-256 `c7cf7b8f18174108d154e4c5d5dde0154fa477a2fc05efa436a28464d98ee3a7`. Existing ingress and cert-manager passed publicly trusted HTTPS checks with that service and embedded browser. This build preceded persistent browser sessions; the following original deployment checks do not establish the cookie flow. Desktop and 390px browser checks exercised project connection, retained merged/open review history, real run details, workers/jobs and read-only Refresh. They observed no browser writes, credential persistence, JavaScript/CSP errors or overflow. A viewport check is not proof from a physical phone.

Actual HTTPS API checks verified authentication, read-only grant enforcement and instance mismatch rejection. The event stream delivered its initial frame and a keepalive after 15.5 seconds through the deployed proxies. Packaged CLI and stdio MCP reads succeeded against the HTTPS endpoint; three CLI reads took 1.16–1.23 seconds from the test machine. These small observations are not throughput or latency guarantees.

The relocation retained every table count and instance identity. A controlled application restart retained the same records and passed readiness again. The host was updated and rebooted before the application deployment; a second full-host reboot after relocation was not performed. This operational proof does not establish automatic backup, failover, native provider dispatch or production-scale capacity.

A later deployment used source `55be5a689717458ae25b8c376a80dda95f5abbca` and packaged `fcd` SHA-256 `8f4b3414400b73da4df455671e9e0669df771656bce03fb6e07f18b53ce3739a`. Trusted-HTTPS desktop and 390px Chromium checks exercised queue, run-detail and worker-stream reload, Refresh and Disconnect, with 118 deliberate browser requests. They verified Secure HttpOnly cookies, absent credential storage and no session resurrection after Disconnect through the deployed proxies. Focused backend tests cover absolute 24-hour expiry; the browser proof did not wait 24 hours or use a physical phone.

Runtime `48fdb5f87de86ec96c955603d7d02d24a8510bed` was later deployed from a verified immutable image after stopping the predecessor and independently inspecting a canonical pre-migration backup. All twenty preexisting table counts, instance identity and event sequence were preserved; forward migrations produced forty-one tables, with integrity and foreign-key checks passing. The first container startup failed because SQLite needed temporary storage; the same exact image failed in an isolated read-only container and passed when only a bounded `/tmp` mount was added. The controller-only temporary volume corrected rollout without replacing the database or weakening its read-only root filesystem.

Sixteen HTTPS API checks and desktop/390px Chromium checks then passed for instance/grant enforcement, jobs/checkpoints, stored review detail, sessions/reloads, Refresh and Disconnect. The browser made 201 requests: 195 reads and six session exchanges, with no other successful writes, JavaScript/CSP errors or overflow. A dedicated stream delivered its initial frame in 272 ms and a keepalive about 13.5 seconds later through the deployed proxies; this proves observed streaming, not exact heartbeat scheduling. The site remained QA with retained imported observations, without provider dispatch, a configured backup schedule, physical-phone testing or operational cutover.

### Recovery-generation QA upgrade

Runtime `3df710996276910811968faf81ef6132a8ddfb7d` was subsequently installed from its verified immutable image. Before the base-schema 1→2 migration, the predecessor was stopped, a canonical backup was exported, and independent local inspection reread the snapshot against the stopped source. The migration preserved instance/signing identity, original receipt bytes, all application-table counts and retained history. Receipt origins were added consistently; the only other changed row content was the managed-state owner stamp. Durable source bytes stayed unchanged through backup, inspection and the final pre-apply check. The existing non-root identity, read-only root filesystem, bounded temporary storage and replacement strategy were retained.

Fifteen trusted-HTTPS API checks and desktop/390px Chromium checks passed for authentication, instance/grant enforcement, stored jobs/checkpoints/reviews, session reload, Refresh and Disconnect. The deployed SSE route delivered its initial frame and an idle keepalive. All four screenshots were inspected without a concrete visual defect; the browser checks reported no errors or horizontal overflow. This is installed QA proof for the exact `3df7109` artifact, separate from the later `ba78122f627f94d284da395396eb0fbdf703e179` full-check build described in [backup proof](backups.md#proof-status-and-limits). The old working controller remains primary pending human GUI review. Stored imports remain historical observations, without automatic live synchronization. Recurring backups, provider resumption, off-host durability, fleet capacity, physical-phone testing, human acceptance and operational cutover are not established.

### Job progress QA upgrade

Runtime `93ad696dbd09d46506e4f6a4ebe3e891e170e957` was installed after its full check/build and six packaged primary-job groups passed. A stopped-source backup was exported and independently inspected locally before startup. Jobs schema two advanced to four, adding five empty tables while preserving all forty-one existing table counts, all forty non-metadata table row hashes, full receipts, identities, history and events. Only the jobs schema value changed in metadata; base schema two and every other metadata value remained unchanged. One ready replica retained the existing security, temporary storage, proxy and node settings.

Twenty HTTPS API groups and desktop/390px Chromium checks passed for the retained views, sessions, current checklist, empty update/checklist archives and primary-job defaults/history. Archives loaded only when opened and stayed closed after reload. The dedicated SSE route delivered an initial frame and idle keepalive; all four screenshots were inspected without a concrete defect, and browser checks reported no JavaScript/CSP errors or horizontal overflow. These installed checks made no successful domain mutations or provider requests. The old controller and site remain primary; native data remains an imported historical observation pending implementation completion and human GUI acceptance. This checkpoint does not establish installed nonempty progress mutations, physical-phone behavior, provider execution or fleet capacity. [Local packaged proof](worker-focus.md#executed-proof-and-limits) exercises the mutation paths separately.

### Retained-data container upgrade check

Before the appearance/coordination QA update, the exact `265af4a5bc48f354516ee73bc578e586f4d2fcde` image passed a disposable historical-data upgrade and restart in 6.275 seconds. The genuine inactive backup first reached the existing 46-table jobs-schema-four baseline under the previous exact image, then advanced to 56 tables and jobs schema six under the candidate. All three daemon starts used UID/GID 65532, read-only root, isolated networking and bounded 64 MiB temporary storage. Existing non-metadata rows, full receipts, signing/instance identity, events and managed state remained exact; only the three selected schema metadata keys changed, and ten new tables remained empty. Integrity and foreign-key checks passed, and restart preserved every aggregate. Source and historical inputs were unchanged; all owned containers and copied data were removed.

An earlier fixture attempt stopped before any daemon start because it expected an empty supplementary-group list; Docker actually repeated the selected primary GID 65532. The corrected fixture requires that exact identity with no foreign group, while the separate host namespace reader retains its own empty-group requirement. The failed attempt remains recorded. This is selected historical-data local container proof, separate from the fresh live baseline, installed HTTPS/browser checks and operational cutover. Native Windows/macOS executable smoke remains a separate release verification gap beyond cross-compilation.

### Appearance and coordination QA upgrade

Runtime `265af4a5bc48f354516ee73bc578e586f4d2fcde` was installed from its verified immutable image after the full combined check/build. A fresh preflight verified the predecessor and absence of native work requiring recovery; a stopped canonical backup was exported and independently inspected locally against the complete stopped source before apply. Jobs schema four advanced to six, adding map/presentation schema one and ten empty tables. All 45 existing non-metadata table counts and full row hashes remained exact, including receipts, auth, identity and history; metadata gained only the two new component keys and changed the jobs version. Events and capture inventory were preserved. The same single-writer replacement, non-root identity, read-only root, bounded temporary volume and proxy/node settings remained in place.

Twenty-nine trusted-HTTPS API checks and desktop/390px Chromium checks passed for retained reviews/jobs, explicit pause/focus defaults and histories, empty notes/map, implicit appearance, scoped links, Refresh, same-cookie reload and Disconnect/reset. Both the incoming-message and independent project-metadata streams delivered initial frames and idle keepalives through the deployed proxies. The browser made 238 requests: 234 reads and four session exchanges, with no successful domain mutations or observed browser errors. All eight screenshots were inspected. Two initial Notes captures showed a transient loading state; a separately preserved narrow follow-up awaited the fresh notes response and settled empty panel at both widths, then inspected two new captures. That follow-up made twenty reads and four session exchanges, with complete owned cleanup. No product change or full-case rerun was needed.

This remains QA with imported historical review observations. The old working controller/site remain primary pending human GUI acceptance and migration/cutover proof. The later source-policy CI-inventory isolation fix has separate source/validation evidence and is not part of this pinned installed artifact. Installed nonempty theme/note mutations, provider execution, physical phones, fleet throughput, human GUI acceptance and operational cutover remain unproven; local controlled matrices cover the nonempty browser paths.

## Activity-based browser renewal QA upgrade

Clean source `ee344a0d36edb98065f6761647c54c7bb1467f44`, packaged daemon SHA-256 `73225828216a05932c29dc426c3de88ea39c50dcc9cdb984fa0884335857424c`, was installed behind the existing trusted-HTTPS ingress. A canonical stopped-source backup was exported and independently inspected before application. The additive project-map migration advanced version one to two and added three empty import-provenance tables; all existing table contents, other metadata, signing/instance identities, receipts, event sequence and captures stayed unchanged. Provider dispatch, recurring backups and public publication remained disabled.

Desktop and 390px authenticated Chromium checks passed in 50.963 seconds, including eight real session renewals, selected-worker/reload continuity, secure cookies, no credential storage, retained nonempty reviews/jobs/map, Refresh and Disconnect. There were no JavaScript/CSP errors, horizontal overflow or domain/provider writes. The [renewal owner](browser-session-renewal.md) records the request counts and client-clock proof limits. An initial unqualified image-tag import was refused before stopping the application; a separately staged fully qualified release passed. No disposable candidate startup/restart experiment or physical-phone test was performed. The old working controller/site remain primary; this upgrade does not establish operational cutover.

## Inter masthead QA update

Clean source `701b6c4873c6f7d6c28e26380383a83e1b55767b` adds the self-hosted Inter semibold masthead, proportional flame and adjusted alignment. Its 280 browser tests and documentation/build checks passed; packaged CLI/MCP/API/upgrade/map-import checks passed in 1.134 seconds and private/public Chromium workloads in 5.541 seconds. The font and its license ship with the website; no font-provider request is needed.

The installed immutable image passed a fresh canonical stopped-source backup, independent local inspection and strict same-schema conservation: all 62 table counts and row hashes, complete schema definitions/versions, signing and instance identities, receipts, event sequence and captures stayed unchanged. Trusted-HTTPS Chromium at desktop and 390px then verified actual Inter glyph rendering and exact font bytes, selected header geometry, nonempty review/map/job views, Refresh and authenticated reload in 29.426 seconds. Its 176 requests were 170 reads, two session creations, two existing renewals and two session deletions; no domain/provider writes, browser errors, clipping or overflow were observed. Both owned sessions were cleared. This is emulated narrow-browser proof, not a physical-phone test, human GUI acceptance or operational cutover.
