# Single-machine offline deployment

[中文](deployment.md)

Basis: [Single-process Cloud (A1)](https://github.com/ora-space/specs/blob/main/decisions/cloud/lifecycle/0-single-process-cloud-on-enterprise-intranet.md) D2/D5/D6 and [Login and sessions (A6)](https://github.com/ora-space/specs/blob/main/decisions/cloud/login/0-idaas-or-email-login-with-shared-web-and-desktop-sessions.md).
A deployment offers only Huawei IDaaS login (`V2_LOGIN=huawei-idaas`), never email. IDaaS login is implemented
but has not yet completed a real login against the intranet IDaaS; do not go live before it has. Database migrations run
automatically at startup. Production deployment
requires a version supporting the required capabilities and a verified HTTPS entrypoint.

## Build online

Use Linux matching the target architecture. Install Go (go.mod), Node >=24, npm, Python 3, Git,
curl, Rust 1.95.0 and a trusted Deno 2.9.5 installation. The desktop repository must contain the
commit pinned in prepare-node.sh.

```sh
DESKTOP_REPOSITORY=/build/desktop V2_NODE_ASSETS_DIR=/build/assets \
  V2_OFFLINE_OUTPUT=/build/cloud-linux.tar.gz task v2:offline:build
```

The builder runs prepare-node.sh first: it compiles Node/reaper at the pinned commit,
checks both Agent plugins against pinned SHA-256 digests, and checks Deno's version before recording
its local digest. Build concurrency is limited to two. Local executable and Deno digests prove
integrity, not publisher authenticity; trust the build machine and toolchain. The frontend uses
offline mode with `VITE_DEMO_MOCK=0`; the build then scans it and stops if any recording demo mock
remains (`npm run demo:check`). Only the server, built web files, five runtime resources,
manifest, launcher and deployment guides are packaged. User configuration, databases, data volumes
and other files from the preparation directory are excluded. Manifest paths are relative; legacy
absolute development manifests remain supported.

Transfer through a trusted channel, unpack into a new version directory and run
`sha256sum --check SHA256SUMS` there. Checksums detect corruption and do not replace trusted delivery
or release signing. Unpacking and startup download nothing. Runtime access to configured PostgreSQL,
IDaaS, model, internal Git and S3 services is still required. Install Git and system libraries on
the target host beforehand; operating system dependencies are not bundled.

## Host and configuration

Use a dedicated non-root account. Agents run as the Cloud account without isolation; the first
release is suitable only for mutually trusted members. Provision a dedicated PostgreSQL database
and identity without demo credentials. Pending versioned migrations run automatically at startup, so
the database identity needs permission to create tables in that database. A database created by a
version before migrations cannot be upgraded and startup refuses it; drop and recreate it.

Keep configuration and secrets in protected process-manager configuration, outside the archive:

- `V2_LOGIN=huawei-idaas`: a deployment lists only this, never `email` (email login has no password, so knowing a member's address is enough to sign in as them; it also marks a development database, see "Database profile" below).
  When `/auth/providers` does not list `email`, the frontend treats the install as a deployment and hides
  only the browser-only model-connections page; the workspace roles page and workflow role fields remain available.
- `V2_PUBLIC_URL`: the public HTTPS address browsers use, the root only, without a path (see "HTTPS ingress"); a path is refused at startup. The IDaaS callback is fixed at
  `<V2_PUBLIC_URL>/auth/callback/huawei-idaas`, derived only from it and never from request headers; startup logs it, and it must
  match the IDaaS application's registration exactly. Cookie-authenticated writes accept only this origin, and the session cookie is
  `__Host-ora_session` (`Secure`). Invitation links are built from it too.
- `V2_IDAAS_CLIENT_ID`, `V2_IDAAS_CLIENT_SECRET`: the IDaaS application's credentials; startup fails without either. Keep the secret in protected configuration only.
- `V2_IDAAS_BASE_URL`: IDaaS origin, default `https://uniportal.huawei.com`; the test environment is `https://uniportal-beta.huawei.com`.
- `V2_IDAAS_API`: which IDaaS endpoint set to call. Default `oauth2` (the set the integration guide and official samples use:
  authorize with `display=page`, JSON token and userinfo requests, failures reported as an `errorCode` in the response);
  `oauth2-v1` is the older `/oauth2/v1/` form endpoint set. Fix it once the site confirms which set works.
- `V2_IDAAS_SCOPE`: default `base.profile`. `V2_IDAAS_DISPLAY_NAME_FIELD`: the userinfo field used as display name, default
  `displayName` (`cn=张三, en=Zhang San` yields the Chinese name, else the English one; without the field the uuid is shown).
- IDaaS console registration: authentication `client_secret_post`, PKCE off; the callback as logged at startup; request the
  `displayName` user attribute (otherwise only the uuid is returned), optionally `uid` and `email`, not the phone number.
  Cloud identifies people by `uuid` alone (account source `huawei-corp`).
- `V2_DATABASE_URL`: dedicated PostgreSQL connection with required TLS and least privilege.
- `V2_ADDR`: plain HTTP listener, default `127.0.0.1:8090`. Only the enterprise HTTPS ingress may
  connect; see "HTTPS ingress" for binding. v2-server does not terminate TLS itself. HTTPS is
  mandatory; never use this HTTP listener as the user-facing entry.
- `V2_DATA_DIR`: an absolute persistent directory such as `/srv/ora/data`. Neither it nor any parent
  may be writable by group or others. Do not use `/tmp`. Prefer mode `0700` owned by the service account.
- `V2_MARKET_CREDENTIAL_KEY`: persist a stable 32-byte Base64-encoded key in the format required by
  pluginmarket and projectrepos. Back it up securely; replacing or losing it makes existing marketplace
  S3 AK/SK credentials, custom marketplace Git credentials, tenant MCP settings, and private project repository tokens unreadable.
- `V2_PLUGIN_MARKETPLACE_URL`: the launcher defaults to empty, disabling the official market.
  Direct binary startup retains the development default when unset; explicit empty disables sync
  and visibility of saved official catalog entries. Custom space markets support S3+AKSK and internal HTTPS Git authentication
  (username plus token or password configured in space plugin settings, sealed with `V2_MARKET_CREDENTIAL_KEY`);
  Git sync disables redirects (`http.followRedirects=false`), so enter the exact current repository URL,
  and changing the URL requires supplying new credentials or explicitly clearing saved ones.
- `V2_PLUGIN_MARKETPLACE_BRANCH` and `V2_PLUGIN_MARKETPLACE_INTERVAL`: used only with the official
  source enabled; defaults are main and 5m.
- `V2_MODEL_*` / `V2_ANTHROPIC_MODEL_*`: see the service README for model connections.
  Changing model connections or credential bindings may prevent
  existing conversations from continuing; notify users and start new conversations while retaining history.

The launcher resolves `V2_FRONTEND_DIR` and `V2_NODE_ASSETS_DIR` relative to itself, checks archive
contents and executes the server. The fixture flags require email login and a deployment start refuses them. `-cleanup` is allowed: stop the service, then run
`start.sh -cleanup` with the same configuration (IDaaS settings are not needed); it removes only registered, confirmed-stopped Node and conversation directories
and keeps repository mirrors and database history. A deployment start skips development presets, demo preparation and demo warming.

**Database profile**: on its first start (or first `admin-invite`) a database records whether it is a development database (email
login on) or a deployment database (off). A later start of the other kind is refused: a development database cannot start without
email (its demo team and preset accounts would appear in the deployment), and a deployment database cannot start with email (it
would receive demo data and passwordless login). To change, create a new database.

## First administrator

A deployment database has no accounts. On the Cloud host, with the same environment as the service, run (`V2_LOGIN=huawei-idaas` is required: this command has no development default, and its first run fixes the database profile):

```sh
./bin/cloud admin-invite -team <team name>
```

It connects to the database, makes sure the team exists, writes a one-time administrator invitation valid for 24 hours, prints it
(`<V2_PUBLIC_URL>/invite/<token>`) and exits without starting the server. The first person to open it, sign in with their Huawei
account and click 加入 becomes the administrator, who then generates member links in Settings → Squads → 成员与邀请. `-team` may
be omitted when exactly one team exists. Use the same command to recover when the only administrator has left. No HTTP route can
create an administrator link.

Install enterprise CAs in the host system trust store using your distribution's enterprise process.
Git and Go HTTPS use system CAs. Never disable certificate verification.

## HTTPS ingress

HTTPS is terminated by the enterprise's existing ingress (gateway or reverse proxy). v2-server does
not configure TLS and the offline package ships no reverse proxy: the IDaaS callback domain and
certificate already belong to the enterprise ingress. v2-server serves plain HTTP on `V2_ADDR` and
does not read forwarding headers such as `X-Forwarded-*`. It takes its public address only from
`V2_PUBLIC_URL`; when that is https, it adds `Secure` and the `__Host-` prefix to the session cookie
and accepts cookie-authenticated writes only from that address.

Ingress requirements:

- Do not rewrite paths: the path the browser requests reaches v2-server unchanged, with no prefix
  added or stripped. Pages, `/api` and `/auth` are all served from the root and the frontend uses
  absolute paths starting with `/`, so the public address must be a root address (for example
  `https://ora.example.com`), not a path prefix. v2-server does not read `Host`, so rewriting it
  does no harm. v2-server checks cookie-authenticated writes against `Origin`, so the
  ingress must not rewrite `Origin`.
- Do not rewrite `Set-Cookie` (no added `Domain`, no changed `Path`): browsers refuse the
  `__Host-` cookies once either is changed, and sign-in does not stick.
- Route to a single v2-server process, with no load balancing across instances: v2-server is a
  single-process service, and it keeps logins in progress in process memory.
- Do not buffer `text/event-stream` responses. Live updates for conversations, task runs, workflow runs
  and workflow nodes use SSE on paths ending in `/chat/conversations/{cid}/events`,
  `/tasks/{taskid}/runs/{rid}/events`, `/workflow-runs/{rid}/events` and
  `/workflow-runs/{rid}/nodes/{nodeRunId}/events` (all under `/api/v2/tenants/{tid}`). v2-server
  already sends `Cache-Control: no-cache` and `X-Accel-Buffering: no`; if the ingress ignores them,
  disable buffering for these paths explicitly, or updates arrive in one burst at the end.
- Serve browsers over HTTP/2. Every open conversation, task run and workflow run page holds a
  long-lived connection; over HTTP/1.1 a browser opens at most 6 connections per address, so with a
  few pages open the remaining requests queue and pages hang.
- Do not cap the total duration of these SSE connections, or make the cap generous. An SSE connection
  stays open while its page is open: a conversation's connection spans turns until the
  conversation's session ends, and a workflow run page's connection lasts until the page is left,
  regardless of how long any model request takes. v2-server sends a heartbeat every 15 seconds, so
  the idle timeout must be well above 15 seconds (at least 60 seconds is advisable). Model requests
  go from v2-server straight to the model service, not through the ingress; `V2_MODEL_IDLE_TIMEOUT`
  and `V2_MODEL_MAX_DURATION` have nothing to do with ingress timeouts. Cutting an SSE connection
  does not stop execution: the browser reconnects within a few seconds, conversation, task-run and
  workflow-node events resume after `Last-Event-ID`, and the workflow run page reads the run again,
  so the page only pauses. If the ingress answers the reconnect with an error (for example a 502
  while v2-server restarts), the browser stops reconnecting and the page stops updating until it is
  reloaded; after a restart, ask users to reload open pages.
- Allow request bodies at least as large as v2-server's own limits. The largest is 6 MiB, for
  workflow definitions, drafts and versions and for workflow run requests such as starting a run,
  submitting a form or sending a node turn; skill and role create, update and import take 2 MiB,
  and the rest are smaller. Do not truncate bodies separately, or saving a large workflow or
  starting a run fails at the ingress.
- IDaaS callback: the callback is fixed at
  `<V2_PUBLIC_URL>/auth/callback/huawei-idaas`, derived only from `V2_PUBLIC_URL` and printed at
  startup; the ingress must forward this path with its query string unchanged.

The hop from the ingress to `V2_ADDR` is unencrypted HTTP. Use HTTP/1.1 on this hop; v2-server does
not speak cleartext HTTP/2 (h2c). When the ingress runs on the same host, bind `V2_ADDR` to
`127.0.0.1:<port>`. When it runs on another host, bind the internal address and use the host
firewall so that only the ingress address can connect to that port, so no other machine can bypass
HTTPS.

## Ingress details to confirm on site

Before deployment, confirm and record with the enterprise network or gateway owner:

- ingress type and version (gateway product or reverse proxy);
- public domain, which becomes `V2_PUBLIC_URL`;
- certificate issuer, whether it is the enterprise CA, and whether the runtime host and employee
  browsers already trust that CA;
- whether HTTP/2 is enabled towards browsers;
- whether buffering can be disabled, the idle timeout set to 60 seconds or more, and the total
  duration limit removed or widened for the SSE paths above;
- request body limit and whether it can be adjusted;
- whether it rewrites paths, `Origin` or `Set-Cookie`, and whether that can be turned off;
- whether it routes to a single v2-server instance;
- where it runs relative to the v2-server host: the same host, or another host and its internal
  address (for the firewall rule).

## First intranet acceptance

Email login or model fixtures cannot substitute for onsite acceptance. Administrator bootstrap is described in "First
administrator" above.

1. Prepare dedicated PostgreSQL and a persistent volume, a registered IDaaS application, an administrator
   and a second employee account, a real project, a working model connection, and an internal Git repository
   containing an installable workflow plugin with its package in S3. Match the package to the target Linux
   architecture and require every entry in `sha256sum --check SHA256SUMS` to pass. Use an isolated acceptance
   instance; do not restart or modify other instances.
2. Install the enterprise CA in the system trust store using the host distribution's enterprise procedure.
   As the actual Cloud runtime identity, use the site's existing DNS/TLS diagnostic tools to check each Git,
   S3 and model host: DNS results must be expected, and HTTPS hostname, certificate chain and validity checks
   must pass. Then exercise actual requests through Cloud below. Never disable certificate validation or
   public-address checks. If internal DNS resolves to an address rejected by the current address policy,
   record a blocker; do not bypass TLS or relax address checks to proceed. Enter credentials only through
   protected configuration or settings, never in command lines, URLs, screenshots or logs.
3. Record the "Ingress details to confirm on site" and configure the ingress and `V2_ADDR` as described
   in "HTTPS ingress". On the ingress host run `curl -sS -m 5 http://<V2_ADDR>/healthz`; it must return
   JSON containing `"status":"ok"`. Run the same command from another intranet machine; it must time
   out or be refused. When the ingress shares the host, `ss -ltn` must list the port only as
   `127.0.0.1:<port>`.
4. On the acceptance instance, bootstrap the IDaaS administrator as described in "First administrator"
   (database migrations run at startup), then invite the second employee to sign in and join the team. In a browser,
   open `V2_PUBLIC_URL` through the ingress and verify that web and `/api` share an origin, the Network
   panel's Protocol column shows `h2`, the IDaaS callback returns to the registered public HTTPS
   address, and the session cookie name has the `__Host-` prefix and the cookie has `Secure`,
   `HttpOnly` and `SameSite=Lax`. Confirm there are no preset demo accounts, projects or content.
5. As administrator, add a custom internal HTTPS Git source under Settings → Plugins → Manage markets.
   Enter its exact current repository URL (no redirects), branch, Git username and read-only token.
   Select S3 signed downloads and separately enter the S3 endpoint, bucket, region and AK/SK.
   Keep Git and S3 credentials separate, and persist and back up `V2_MARKET_CREDENTIAL_KEY`.
   Synchronize and confirm no source failure and the expected catalog version.
   Install a workflow plugin and confirm its imported workflow opens on the workflows page.
6. Create a separate acceptance source using a different repository address, not another URL spelling of
   the same repository. Synchronize successfully first, then replace only this source's Git token with an
   incorrect value and synchronize. Require the internal Git server's authentication failure response to
   be recognized as a readable repository authentication failure (not a generic network or clone error)
   and confirm the previous catalog is retained. Clear the saved Git credentials and synchronize again: a
   repository requiring authentication must still fail explicitly and retain its catalog. Restore correct
   credentials and verify synchronization succeeds again. Leave the normal source untouched; do not change
   S3 keys to mask a Git failure.
7. Run the installed workflow and complete a conversation on the real project. Verify the actual model,
   package download and Node execution succeed and that results and history are readable. Watched
   through the ingress, conversation and workflow output must appear as it is generated, not in one
   burst at the end. Then open a conversation page, filter the browser developer tools' Network panel
   by `events`, and keep the page open longer than the ingress idle timeout and total duration limit
   recorded in step 3 (the larger of the two plus a few minutes). Pass: the conversation has exactly
   one `events` request and it is still open. If the ingress's total limit cannot be removed, the new
   `events` request that appears when it expires must carry a `Last-Event-ID` request header and
   return 200, and the page must show no missing or duplicated output. Record members, installed
   plugins, history and an acceptance file on the data volume, then restart only the acceptance
   instance. After login, verify those states and files remain, the key still decrypts source
   credentials, and synchronization succeeds again. Unfinished executions from before restart should
   be interrupted; automatic resumption is not a success criterion.

Keep redacted evidence for every step: program version, platform architecture, SHA verification, time,
action and result. Never record secrets, tokens or full authentication responses. On failure, distinguish
DNS resolution, TLS trust/hostname, Git or S3/model authentication, package download/digest verification,
and the Node/workflow execution chain in that order. Record safe error codes and redacted messages and
stop the affected step. A retained catalog or fixture output does not prove successful synchronization,
installation or real execution.

## Backup and upgrade

Back up PostgreSQL, the complete `V2_DATA_DIR` volume and the stable encryption key. Stop the service
or use consistent snapshots, and test restoration. Agent changes exist only in local working trees
and are not pushed to Git; a database-only backup is insufficient.

Stop this instance and confirm its Nodes exited. Back up, unpack a new version into a separate
program directory and verify checksums. The new version runs migrations automatically at startup, so
back up before upgrading; a failed migration stops startup at the last completed one, and the
error lists the migrations this start already applied. A database created by a version before
migrations must be dropped and recreated; startup refuses it, as it
refuses a database with migrations newer than the binary or an applied migration that changed. Replace
programs and resources while retaining database, data directory, keys and host configuration. Do
not reset demos, seed again or overwrite new resources with the old package. Unfinished executions
become interrupted on restart and do not resume automatically. After migration, rollback requires
a verified backup recovery procedure, not merely an older binary. Static files use `no-cache` so
browsers revalidate after upgrades.
