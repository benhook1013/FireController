# Offsite backups

## Implementation status

The optional rclone push, compact status and fetch commands are implemented. Packaged plain and encrypted round trips pass against loopback S3 and an independent Docker MinIO fixture, including cloud retention, authentication failure, corruption refusal and inactive recovery with source data removed. Focused race tests cover uncertain publication, restart, cancellation and ownership boundaries. Real OneDrive/Google Drive/MEGA authorization and cloud recovery remain pending until an account is connected. Existing local backup creation and retention remain unchanged.

## One backup format, optional transport

The operator runs `fcd backup-push --config /private/offsite.json` separately from the serving daemon, normally through a deployment timer. It selects the newest completed bundle for the explicitly configured instance from the existing local backup root. It validates that bundle, prepares a deterministic compressed archive, uploads it through a configured rclone remote, downloads it again and verifies the archive and native bundle before publishing a completion receipt. Provider traffic never occupies the serving database or an agent command. Cloud failure preserves local backups and the previous verified offsite success.

The remote may be OneDrive, Google Drive, Dropbox, MEGA, S3-compatible storage or an rclone crypt remote. FireController does not implement their authentication APIs. Provider credentials and optional encryption keys stay in a private rclone configuration, outside Git and backup payloads. Configuration names the executable and configuration file explicitly; ambient rclone options do not silently select a destination. Browser OAuth and provider-specific unattended authentication must be verified for each installed remote. Keep encryption recovery material independently of the source server.

## Completion, retries and retention

Each instance has its own remote prefix. Each backup has an archive and a small versioned completion receipt binding its ID, source instance, snapshot time, event sequence and archive size/SHA-256. A directory or object appearing in storage is not evidence of a complete backup. Fetch validates the receipt, archive and native bundle before returning a usable local copy. Loss of the source server does not require its transfer ledger to fetch an explicitly selected remote backup.

One local state directory admits one transfer at a time. Its compact state binds the destination and instance, remembers the latest attempt and verified success, and retains the owned backup receipts needed for retention. A single pending archive/receipt is recorded before network work; retries and restart finish that same publication before selecting a newer source. A lost upload response does not produce another backup identity. An unchanged latest source does not upload it again. Operation deadlines, bounded process output, source/archive sizes and ownership inventories limit work and disk use. A reserved, connection-marked workspace is reclaimed under the transfer lock after interruption; unrecognized directories are preserved and block the operation. Durable pending intent allows restart on either side of the prepared-archive rename without the source bundle. Status reports the last persisted attempt; a `running` attempt after a process kill is not a live heartbeat, and the next push resumes the pending publication.

Cloud retention is independent of local retention. It runs only with a verified newest remote copy and removes only exact archive/receipt objects recorded as owned for that instance and destination. It never mirrors local deletion, runs a root purge or adopts arbitrary remote files for deletion. Unknown, changed or conflicting objects fail safely. A retention failure remains visible alongside the successful upload and can be retried. A bounded retained inventory stops further uploads if unresolved cleanup would otherwise grow without limit. Preserve the private transfer state to preserve automatic retention ownership: a fresh state directory can fetch existing backups, but does not adopt their objects for deletion. Old instance namespaces remain untouched after restoring under a new instance identity.

## Operator commands

```sh
fcd backup-push --config /private/offsite.json
fcd backup-offsite-status --config /private/offsite.json
fcd backup-fetch --config /private/offsite.json --id backup_0123456789abcdef0123456789abcdef --destination /private/recovered
fcd backup-inspect --bundle /private/recovered/backup_0123456789abcdef0123456789abcdef
```

Omit `--id` to fetch the latest verified publication. Its independently checked `latest.json` pointer is written only after the archive and completion receipt have been verified. Fetch writes a new destination and does not activate a restored service. Use the existing [restore procedure](backups.md) after verification; its fencing, new identity, credential revocation and provider holds still apply. Status reads compact local transfer facts without contacting storage. A past verified success is not a claim that remote bytes are presently intact.

The private version-one configuration selects `source_root`, `instance_id`, `state_dir`, `rclone_binary`, `rclone_config`, `remote`, `keep`, `timeout` and `max_bundle_bytes`. Use a dedicated remote subdirectory and state directory for each source instance/destination pair. `keep` is an explicit count from one through one hundred; timeout is positive and at most twenty-four hours. The byte limit bounds both the downloaded archive and expanded bundle. A new destination or instance requires a new state directory; editing provider credentials must not silently repoint a named remote to unrelated storage.

### Installation and authentication

Start with the [configuration example](../examples/offsite/offsite.json), [service](../examples/offsite/firecontroller-offsite.service) and [timer](../examples/offsite/firecontroller-offsite.timer). Install a matching `fcd` and a separately installed rclone executable; rclone 1.75.2 is the qualified version and CI verifies its published Linux archive against a pinned SHA-256. The serving container does not acquire an automatic rclone dependency. A separate host service or uploader container can use read-only access to completed bundles and its own private writable state/configuration. No daemon restart, live database mount, FireController API credential or agent profile change is needed for this process.

Select the OS user/group that already owns the completed backup bundles, adjust the example paths and source instance identity, and create its offsite directory with mode `0700` and both configuration files with mode `0600`. Keep the state/configuration directories outside the source backup root. Allow rclone to update its own private configuration directory so OAuth token renewal can persist. Do not make the private backup root world-readable to share it with an uploader. Start the timer only after the manual cloud round-trip proof below succeeds. The timer runs on the server; the operator's PC need not stay online.

Use rclone's [headless authorization](https://rclone.org/remote_setup/) to connect OneDrive, Google Drive or Dropbox: prepare configuration on the server, complete the provider's browser consent on the operator's PC and transfer the resulting authorization through the private setup channel. Do not put tokens in chat, source control, shell command arguments or logs. MEGA's email/password and optional 2FA follow its [backend guide](https://rclone.org/mega/); verify unattended reauthentication before treating that connection as production-ready. An [S3 remote](https://rclone.org/s3/) uses a provider endpoint and scoped access/secret keys in the private configuration. Local fixture credentials never establish a real cloud account connection.

For client-side encryption, configure an [rclone crypt remote](https://rclone.org/crypt/) over a dedicated provider folder or bucket prefix and select that remote in `offsite.json`. Retain the encryption password/salt or a recoverable private rclone configuration independently of the server. Cloud sign-in alone cannot recover a lost encryption key. Rclone configuration-file encryption requires a separate noninteractive unlock arrangement and is not accepted implicitly through ambient agent environment variables; the initial unattended setup uses a private owner-only configuration file.

Before enabling scheduling, run one push, inspect compact status, fetch the latest copy into a new private directory, run `backup-inspect`, and restore it into a new inactive directory. Verify the source identity, useful retained records and expected inactive recovery state. Repeat this against each real provider being claimed operational; a successful S3 fixture does not prove Microsoft/Google/MEGA authorization, account quota or cloud durability. Authentication expiry or provider failure must leave the previous verified offsite success and ordinary local backups intact.

### Local qualification

After building `bin/fcd`, run `make offsite-smoke RCLONE=/absolute/path/to/rclone`. The workload creates a disposable loopback S3 server and synthetic credentials, invokes the actual packaged push/status/fetch commands, verifies independent cloud retention, rejected-credential retry and corrupted-download refusal, and downloads/restores both plain and crypt-wrapped bundles after removing the source database and local backup root. CI runs this workload with the pinned transport; ordinary focused Go tests cover uncertain publication, corruption, cancellation, ownership and state binding without external tools.

To check an independent local S3 implementation, supply `FC_TEST_S3_ENDPOINT=http://127.0.0.1:PORT` and synthetic `FC_TEST_S3_ACCESS_KEY`/`FC_TEST_S3_SECRET_KEY` for a disposable MinIO or equivalent fixture before invoking the same target. The workload refuses non-loopback external fixture endpoints. Bound Docker ports to loopback and remove the owned fixture afterward; no real account or hosted-controller data is needed. The integration test does not claim a provider passed merely because its backend appears in rclone's supported list.

## Required proof

Exercise actual rclone upload/download against a disposable local S3-compatible service, including encrypted transport, unchanged-source retry, restart after an uncertain publication, independent retention, corruption, permission/authentication failure and bounded cancellation. Download and inspect a complete native bundle and restore it inactive with source data unavailable. Keep fixture credentials and containers private and remove owned test resources afterward. These checks establish the common transfer boundary, not AWS-specific behavior, physical durability or real OneDrive/Google Drive/MEGA account authorization. Live cloud upload/download/restore remains pending until an account is connected.

Google account connection and maintainer branding are described in [Google Drive setup](google-drive-setup.md).

## Website reporting

An operator can publish a small redacted report from the existing local transfer ledger:

```sh
fcd backup-offsite-status --config /private/offsite.json --health-file /private/reports/onedrive.json --health-label OneDrive --health-max-age 3h
```

`--health-file` is accepted only by the status command. The optional label defaults to `Offsite backup`; the maximum snapshot age defaults to three hours and can be selected from one minute through thirty days. These reporting options never start an upload, download or remote verification. Existing status output and transfer ownership remain unchanged.

Publish into a dedicated existing private directory and make the resulting owner-private regular file readable by the daemon's operating-system user. Mount only that redacted directory into a container, read-only. Never mount the rclone configuration or transfer state into the serving daemon. Select each report explicitly with repeated daemon `--backup-offsite-health-file /private/reports/onedrive.json` flags, up to eight reports. Use a small deployment timer to republish once a minute; reporting older than five minutes is unavailable even if the last retained snapshot is recent. A publisher failure leaves the prior report to age out. The mirror binds the source instance, so a restore cannot silently claim a predecessor's offsite protection.

The site separately considers snapshot age, recent failed attempts and retention failures. An unchanged transfer does not reverify remote bytes, and publishing a new report does not update the successful snapshot's time. The status is evidence of recorded successful backups, rather than continuous cloud integrity verification.
