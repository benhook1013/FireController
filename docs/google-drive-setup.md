# Google Drive backups

## Implementation status

The offsite transfer and recovery commands use rclone. Public product and privacy content is available from the embedded information handler at `/about` and `/privacy` when those routes are installed. The maintained Google OAuth registration, its production publication, the packaged account-connection helper and the installed Google Drive authorization/upload/recovery proof must be completed before claiming Google Drive ready for ordinary users. A configured OAuth client without a user token does not establish account access or a working backup.

The qualified rclone 1.75.2 transfer binary is not itself a complete implementation of the selected connection experience. Its [OAuth implementation](https://github.com/rclone/rclone/blob/v1.75.2/lib/oauthutil/oauthutil.go) checks random state but does not exchange a PKCE verifier. The supported connection helper must use the standard OAuth library with PKCE and strict loopback/state checks, then install the token in the private rclone configuration for subsequent transfers. This is a connection-setup boundary; upload/download qualification does not prove it.

## Connect an account

The supported user experience uses a FireController-maintained Google OAuth registration. An operator starts the connection helper on a trusted computer, opens Google's sign-in page, chooses the account and consents to backup access. A browser-capable local helper receives the loopback callback and securely installs the resulting private authorization on the backup host. Google Cloud Console setup, app branding and client creation belong to the publisher, not to every person connecting storage.

Request only `https://www.googleapis.com/auth/drive.file`. It allows app-created or explicitly selected files and is classified by Google as non-sensitive. Rclone supports this scope and cannot use it to enumerate arbitrary pre-existing Drive files. Create a dedicated backup folder through the connected application and pin that folder ID as the remote root. Do not widen the permission to full Drive access to reuse an unrelated existing folder. See [Google's scope guide](https://developers.google.com/workspace/drive/api/guides/api-specific-auth) and [rclone's scope explanation](https://rclone.org/drive/#drive-file).

Authorization tokens remain private on the setup computer and selected backup host. They are never published in project records, copied into backup archives or sent through public information pages. Use the [offsite backup procedure](offsite-backups.md) for optional encryption, retention, timer installation and an actual upload, independent fetch, inspection and inactive restore. Successful consent alone is not backup qualification.

## Publisher setup, once for FireController

The maintainer owns a Google Cloud project with Drive API enabled and a Desktop OAuth client for the installed helper. Desktop clients are public clients: a shipped client identifier does not authorize an account, and a bundled desktop client secret is not a server security boundary. Keep user tokens private. The helper uses the system browser, a loopback callback, random state and PKCE through the standard OAuth library; a confidential web application client must not be distributed as a substitute. See [Google's desktop application flow](https://developers.google.com/identity/protocols/oauth2/native-app).

Complete the registration's Branding fields with the real application name, a reachable support email selected from the maintainer's account, a public product homepage and the matching public privacy page. The hosted FireController deployment provides `/about` and `/privacy` for these links. Add and verify the registrable domain through an account associated with the project when Google requires ownership proof. Do not invent an email address, substitute an unrelated privacy policy or require each self-hosted user to publish their own branding. Google's [brand verification guide](https://developers.google.com/identity/protocols/oauth2/production-readiness/brand-verification) owns the current publication requirements.

Only the selected Drive scope is needed; do not add account identity, Gmail or full Drive scopes. The scope's non-sensitive classification avoids sensitive/restricted-scope review for that scope; it does not waive Google's publication and branding requirements. Follow the console's actual verification outcome before describing the application as verified. External Testing authorization has a limited refresh-token lifetime for Drive access and is unsuitable as the final unattended-backup setup; see [Google's token-expiration rules](https://developers.google.com/identity/protocols/oauth2#expiration).

Maintain production credentials separately from development/test clients and environments as required by [Google's production policy](https://developers.google.com/identity/protocols/oauth2/production-readiness/policy-compliance). A self-hosted installation can use the maintained Desktop client through the local helper without registering a new website callback. Operators choosing a separate custom OAuth application own its registration and policy obligations.

Rclone's shared Google client is being retired during 2026 and is not a dependable fallback for this product. Use the maintained FireController client rather than sending normal users to [rclone's custom-client instructions](https://rclone.org/drive/#making-your-own-client-id).

## Disconnect and recover

Disable the backup timer, revoke the connection in Google Account settings and remove saved tokens from the setup computer and backup host. Existing remote backups remain until the operator removes them; revocation does not erase backups. Keep independent encryption recovery material when crypt is enabled. A newly configured client or transfer ledger does not automatically acquire deletion ownership of existing backup files.
