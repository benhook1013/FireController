# Embedded Browser Guide

## Implemented boundary

`fcd` serves a read-only project browser from the same origin as its API. It displays project identity, paginated workers and basic jobs, explicit full job briefs, worker inboxes and explicit conversation pages. Incoming messages update through the persistent [message stream](message-events.md). Reads never mark messages seen or acknowledged, edit jobs or contact GitHub. The existing development dashboard remains separate until replacement and migration proof are complete.

The [browser design](browser.md) owns the broader target. Maps, review queues, checkpoints, project appearance settings and browser mutations are not implemented by this slice. The default fire palette and dense responsive layout apply until project presentation records are implemented. A small repository attribution remains in the footer.

## Connect and navigate

Run `fcd`, open its printed loopback address and enter a project ID and a credential with read access to that project. Obtain these through the existing [project setup API](local-api.md#project-and-worker-setup); this slice does not add an onboarding wizard. Each connection verifies the service instance and selected project before loading project data.

The credential lives only in that browser tab's active memory. It is absent from URLs, browser storage and query-cache keys; submitting clears the password field. Disconnect, failed authorization or a changed project route cancels pending requests and removes the prior session's cached data. Reload requires a fresh credential. A canonical project detail link preserves its path when reconnecting to the same project.

Routes are `/projects/{project_id}`, `/projects/{project_id}/jobs/{job_id}`, `/projects/{project_id}/workers`, `/projects/{project_id}/inbox` and `/projects/{project_id}/inbox/{conversation_id}`. Choose a worker to follow its incoming inbox. Worker changes cancel the prior view's requests and subscription and clear its cache; they do not alter credentials, assignments or server state. Project-scoped read access permits conversation reads within that project, as documented by the API.

“Refresh visible data” rereads only active stored-data queries. There is no timer-based JSON polling, focus-triggered refresh or provider refresh. Jobs and workers update when explicitly refreshed. Only incoming message changes currently have live transport; a connected inbox indicator does not claim that jobs, reviews or maps are live.

## Live delivery and failures

The browser takes an atomic inbox snapshot and its event cursor, then opens authenticated same-origin fetch SSE. Cursor advancement follows successful application to the view. Committed events invalidate relevant active inbox/conversation reads; transport keepalive comments do not trigger reads or appear as activity.

Ordinary API reads share a five-second budget per operation. Stream connection headers have a five-second budget; an established stream stays open with its own cancellation signal. The parser bounds frames to 1 MiB and validates UTF-8, scope, metadata shape and cursor identity. It rejects malformed events rather than exposing arbitrary response text.

After a transient disconnect, the view shows reconnecting and retries stream attachment at capped delays of one, two, four and ten seconds. The last processed cursor survives those attempts in memory. Expired replay requires a fresh snapshot before resubscribing. Access errors terminate delivery and clear the session. A stream with no traffic for 45 seconds is reattached; routine server keepalives keep a healthy idle connection open. Canceling or disconnecting never acknowledges messages.

Project, worker and connection nonce scope query keys. Old-context results cannot populate a new view. Returned text is escaped by React; the app accepts no raw HTML, external images, inline scripts or unrestricted styles. Embedded responses set a same-origin content security policy. Unknown API paths and unavailable assets remain JSON errors rather than receiving application HTML.

## Build and packaging

The client uses React, TypeScript, declarative React Router and TanStack Query. Vite builds a client-only application; there is no Node server at runtime. Exact dependency versions and the browser lockfile are committed in `webapp/`.

Install both lockfiles and run the repository checks:

```sh
npm ci --ignore-scripts --no-audit --no-fund
npm --prefix webapp ci --ignore-scripts --no-audit --no-fund
make check build
```

Use Node.js 22.22 or newer for development. `make check` type-checks the app and runs focused API/SSE tests alongside Go race/vet and documentation checks. `make build` regenerates `internal/webui/assets` and embeds them in `fcd`. CI rebuilds the assets and rejects tracked differences or new untracked output. Commit the generated index, fingerprinted JavaScript/CSS and bundled third-party licenses with their source changes. Source maps and package manifests are not served. Bundled notices are available at `/third-party-licenses.txt`.

Committed assets allow direct `go build ./cmd/fcd` or `go install` without Node; these commands use the checked-in browser build rather than regenerating it. The development Vite proxy targets `127.0.0.1:8080`; Vite results do not substitute for embedded-service browser proof.

## Proof and limits

At the original packaged-browser checkpoint identified by the source and binary hashes below, `make check build` passed: TypeScript, nineteen browser tests, Go race tests/vet, nineteen documentation checker tests and asset generation. The nineteen-browser-test count belongs to that historical checkpoint, rather than the current expanded suite. Go handler tests cover canonical routes, MIME/cache headers, HEAD, unsupported methods, traversal, safe errors and restricted embedded files. Browser tests cover bounded scoped API responses, incremental SSE parsing, cancellation/replay/authorization failures, and event refresh while the first read is pending.

Seven actual packaged-service Chromium groups passed on Linux/WSL. They exercised connection/authentication, explicit job/conversation reads, live incoming messages, worker/project switches with held old responses, a committed reply arriving during the first conversation read, real service stop/restart and resumed delivery, and 390-pixel phone/deep-link layouts. The delayed first response could not swallow or overwrite the reply; a fresh read displayed it without another event. Desktop and phone screenshots were visually inspected after correcting headline spacing and attribution layout.

The run observed 46 browser requests, 40 API requests and six stream attachments across intentional selections and restart. There were zero browser mutations, JavaScript/CSP errors or horizontal overflow. A settled connection made zero JSON requests during a 16.2-second idle window and kept one active stream. Credentials stayed out of browser storage and URLs; synthetic HTML-like bodies remained literal text. Browser reads used read-only grants, with durable message/seen/ack state checked around intentional fixture mutations. Setup alone used public GitHub repository verification; no direct provider-call counter was measured, so the no-provider-work boundary also relies on the inspected local read handlers.

The complete 79-file Go/module/browser/config/lock/generated-asset manifest was unchanged before/after this run: SHA-256 `ca9360ee46f1f23be8df1072b7c91f5c2282be9b2aba9f18c35df6e6c2af0075`. The binary hash was `e93015a74d894d5b836331c3e2aaa85513ade64bddca448f45a92ec79c108b05`. This identifies tested source bytes rather than implying proof for later edits. The run used Playwright 1.64.1 with an available Chromium 153.0.8010.12 headless shell; that cached browser differs from the driver's preferred revision. Disposable state and owned processes were removed.

This slice does not establish remote TLS deployment, a public projection, multi-tab identity synchronization, native platform browser coverage, accessibility conformance or fleet throughput. Credentials are private connection material; the application does not install them in the operating system or manage a human login session.
