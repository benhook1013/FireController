# Container Runtime

## Supported image

The root [Dockerfile](../Dockerfile) builds the Linux/amd64 daemon from Go source and committed embedded browser assets. It uses the official Go 1.27.2 Bookworm builder and Debian Bookworm slim runtime, with immutable image-index digests as defaults. The runtime installs Git, its HTTPS transport dependencies and CA certificates through Debian's signed package repositories, retaining package copyright files. It runs as UID/GID 65532 with `/fcd` as its entrypoint. No GitHub credential, operator catalog, provider CLI or CLI authentication context is included.

The source build uses `CGO_ENABLED=0`, `-trimpath` and `-buildvcs=false`. Preserve the selected source commit and resulting image digest as release evidence: embedded VCS metadata does not identify this archive/context build. The build context allowlist includes only Go source, embedded assets, module files and the license; `.git`, `.local`, host profiles, node_modules and private captures are excluded. Do not place secrets in source files or pass them as build arguments.

```sh
docker build --platform linux/amd64 -t firecontroller:provider-runtime .
```

Node.js is not needed for this image build. Browser changes must first regenerate and validate the committed embedded assets through the [normal build workflow](development.md#build-and-checks). Module downloads use `proxy.golang.org` without a direct-source fallback and verify the Go checksum database. Base images are pinned, but signed Debian package updates are resolved at build time; record the final image digest and installed package versions rather than claiming byte-identical rebuilds. Deliberate base/toolchain upgrades can override `GO_IMAGE` and `RUNTIME_IMAGE` with reviewed tag-and-digest references using `--build-arg`.

## Data, identity and networking

Run exactly one writer against a private persistent data directory. Stop its predecessor before starting a replacement; do not overlap replicas against SQLite. Back up and verify retained data before upgrades as described by the [backup owner](backups.md). Rootless Docker and user namespaces require the operator to map the container UID to the correct host volume owner.

For a Linux host without user namespace remapping, create a dedicated directory with the container's numeric owner:

```sh
sudo install -d -m 0700 -o 65532 -g 65532 /srv/firecontroller/data

docker run -d --name firecontroller --platform linux/amd64 \
  --network host --read-only --user 65532:65532 \
  --cap-drop ALL --security-opt no-new-privileges \
  --tmpfs /tmp:rw,nosuid,nodev,size=67108864,mode=1777 \
  --mount type=bind,src=/srv/firecontroller/data,dst=/data \
  firecontroller:provider-runtime
```

The default command uses `/data` and `127.0.0.1:8081`. Host networking in this Linux example makes that loopback address reachable by a same-host TLS reverse proxy; publishing a Docker bridge port does not make a daemon bound to the container's loopback reachable. A same-pod proxy can instead share the daemon's network namespace. Keep the application listener loopback-only. The [hosting contract](hosting.md) owns proxy headers, HTTPS termination, buffering, ownership and single-writer replacement. The inbound TLS certificate belongs to the proxy; CA certificates in this image validate outbound HTTPS.

The root filesystem may stay read-only. `/data` must be writable and private; `/tmp` supplies bounded writable SQLite temporary storage. The initial bootstrap credential is written privately under `/data`; obtain it through the explicit administrator setup procedure, never through image layers or access logs. The default empty catalog starts no native provider workers, and history collection, scheduled backups and public publication remain opt-in.

## Optional review capabilities

Both hosted and CLI review channels require Git: each materializes and verifies its own pinned source workspace under `/data/review-workspaces`. Hosted execution uses the service's GitHub API adapter and needs no `gh` or CodeRabbit CLI executable. Real execution additionally requires working DNS/TLS/egress to the configured provider, appropriate repository/account authorization, current source facts, project configuration and policy, and proven included allowance. The image does not grant any of those authorities.

Select a private catalog explicitly by appending `--data-dir /data --listen 127.0.0.1:8081 --review-catalog /operator/catalog.json` to the run command and mounting its directory read-only. Each catalog/credential file must be an owner-private regular file readable by UID65532; use that ownership with mode0600 on Linux. Prefer mounting the whole owner-private directory at `/operator` so relative credential references remain bound to it. No credential or catalog is discovered automatically. The optional `--github-token-file` for repository/history reads is separate from a capability's `github_credential_file`.

CLI review requires an operator-provided compatible CodeRabbit executable, its interpreter/shared libraries if required, and a protected authenticated home/configuration context. Install them in a deliberately derived image or mount the complete required runtime read-only; mounting one executable is insufficient when its runtime dependencies are missing. Bind its authentication directories explicitly through the catalog's `authentication_environment`; they are separate from the service GitHub token and other projects' profiles. Only operator-selected home/configuration paths are supported. Any writable CLI authentication/cache paths must be explicit private mounts; the base image does not configure or test them. Native CLI execution requires supported Unix process-group ownership.

Establish a separate service authentication context rather than copying another worker's entire CLI home, review cache or mutable login files. CodeRabbit's [CLI reference](https://docs.coderabbit.ai/cli/reference#auth-login) documents `coderabbit auth login --agent --no-browser`: complete browser OAuth against the service's localhost callback, forwarding that port when logging into a remote host. Run login and `coderabbit auth status --agent` with the same private home/configuration mounts and numeric owner selected for the capability. Authentication establishes identity, not included allowance; the native pipeline still checks allowance before every review. The isolated executable check for CodeRabbit 0.9.0 passed in this Debian runtime with networking disabled and an empty temporary home; it did not sign in, inspect an existing login, or perform a review. Real service authentication remains a separate deployment check.

The [execution contract](review-execution.md#operator-capabilities-and-project-configuration) owns exact catalog fields, fingerprints, actor identity, allowance, reservations, recovery and bounded provider actions. Changing image contents does not authorize a review or bypass restore holds. This image supplies dependencies; a real provider review and retained-data migration require their own proof.

## Verification boundary

Local image qualification checks Git version, HTTPS helper and CA availability, retained copyright files, numeric non-root ownership, read-only-root startup with bounded temporary storage, and empty-catalog daemon readiness on exclusively disposable data. The Linux CI build also compiles the image and checks its default non-root identity, Git HTTPS helper, CA bundle and bare-repository creation without network access. A separate credential-free public Git HTTPS smart-protocol read passed using the same read-only non-root runtime image, verifying two selected references without API or review requests. Configured hosted-only startup also passed with a private synthetic catalog and relative credential file while networking remained disabled: readiness succeeded, all 47 review/collection tables stayed empty, and the daemon stopped cleanly. This does not establish private-repository authorization, CLI authentication, completed native reviews, hosted deployment or cutover.
