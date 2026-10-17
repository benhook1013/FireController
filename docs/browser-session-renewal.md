# Browser Session Renewal

## Implementation status

Rolling renewal replaces the fixed daily logout. Implementation and focused proof are in progress; the installed QA build still uses fixed 24-hour sessions.

## Selected behavior

A valid private browser connection can renew its existing session for another 24 hours without asking for the original API credential. A visit, returning to a visible page, or explicit keyboard/pointer use counts as activity. A new connection already issues a fresh cookie. The client renews on a successful resume and at most once per five minutes while such activity continues. Quiet SSE traffic, keepalives, background invalidations, timers alone and a hidden tab do not renew it. A visit within the valid window therefore keeps access available; a session already expired requires login. The small renewal throttle means the idle boundary is approximately 24 hours since last use.

`POST /api/v1/projects/{project_id}/browser-session/renew` takes exactly `{}` and the existing selector/cookie/instance binding. It requires an exact same-origin Origin, including rejecting a missing Origin, and current read authorization. It returns the existing six identity/capability fields and replaces only that session's cookie. It does not accept a bearer credential, change the selector, create another session, grant a new permission, write a domain record/receipt/event or contact a provider. Private browser metadata retains the returned expiry only after a validated successful response.

The renewed encrypted cookie retains the original write and administrator allowances; effective flags still intersect current grants. Cookie security and host binding remain unchanged. Revocation, project changes, mismatched instance/selector, tampering and expiry deny renewal. A later credential promotion cannot broaden the original session. Current authorization is checked again at renewal. Logout expires the selected cookie as before.

Renewal uses one bounded finite request, with no overlapping renewals, automatic retry loop or credential persistence. A transient failure preserves the connection and may be tried again on a later activity. A dispatched renewal can set the cookie even when its response body is lost: stored expiry is an informational hint, and explicit resume validates the cookie through the current-session GET before saving the returned expiry. Hint timestamps cannot grant access; an actually expired or revoked cookie still requires login. Terminal authorization/instance failures end access normally. Late results from a disconnected/replaced session cannot update its successor's metadata. Successful renewal, or an uncertain dispatched result, retires only the old private message/project transports. Retired responses cannot report terminal expiry against their successor. New transports authorize the current cookie and preserve processed replay cursors; it does not reread unrelated archives or acknowledge messages.

## Proof boundary

Focused service proof covers unchanged selector/security/initial allowances, current authorization and expiry, strict body/Origin/authentication, project/tab isolation and absence of domain/provider effects. Client proof covers activity admission/throttling, one in-flight request, finite timeout, transient/terminal failure and late-session cancellation. Actual packaged browser proof must exercise valid renewal, preserved connection/reload state, inactivity and revoked access without relaxing production defaults. Focused clock-controlled service tests establish the exact expiry boundary. Deployment and physical-phone observations remain separate.
