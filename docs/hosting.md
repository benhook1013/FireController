# Hosting and HTTPS

## Supported boundary

One FireController service serves the API, live event transport and embedded browser. Local and remote installations use the same application and project permissions. Hosting remotely does not make private project data public. The optional public projection in [browser privacy](browser.md#privacy-and-public-status) remains unimplemented.

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

The implemented browser feed uses SSE over HTTPS. Disable response buffering and allow idle intervals longer than its fifteen-second transport keepalive at every proxy. Verify actual frame delivery through the deployed route; a successful page load alone does not prove streaming. WebSocket transport and remote MCP endpoints remain separate, unimplemented capabilities.

Before relocating an instance, stop its existing writer, capture a consistent database snapshot and required private files, and verify integrity, identities and retained records at the destination. Switch clients only after destination checks pass and keep the predecessor stopped. A stale predecessor is not a rollback database after new writes reach the destination. The [architecture recovery contract](architecture.md#storage-operations-and-growth) owns disaster restore, credential rotation and fencing; a planned relocation is distinct from disaster restore.

The managed backup API and automatic off-host backup remain unimplemented. Migration snapshots and host persistence do not establish that backup capability.

## Executed deployment proof

A single-server deployment using existing ingress and cert-manager passed publicly trusted HTTPS checks with the packaged service and embedded browser. Desktop and 390px browser checks exercised project connection, retained merged/open review history, real run details, workers/jobs and read-only Refresh. They observed no browser writes, credential persistence, JavaScript/CSP errors or overflow. A viewport check is not proof from a physical phone.

Actual HTTPS API checks verified authentication, read-only grant enforcement and instance mismatch rejection. The event stream delivered its initial frame and a keepalive after 15.5 seconds through the deployed proxies. Packaged CLI and stdio MCP reads succeeded against the HTTPS endpoint; three CLI reads took 1.16–1.23 seconds from the test machine. These small observations are not throughput or latency guarantees.

The relocation retained every table count and instance identity. A controlled application restart retained the same records and passed readiness again. The host was updated and rebooted before the application deployment; a second full-host reboot after relocation was not performed. This operational proof does not establish automatic backup, failover, native provider dispatch or production-scale capacity.
