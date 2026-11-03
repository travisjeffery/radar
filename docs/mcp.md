# Hosted Radar review MCP

`radar-mcp` runs Radar's CI review on any commit pushed to an allowlisted
repository, before a pull request exists. It is an MCP server meant to run
behind an OAuth proxy that signs people in with your identity provider. It is
report-only: it never comments on, approves or changes anything on GitHub.

## Using it

**Claude Code**

```sh
claude mcp add --transport http radar https://radar-mcp.example.com/mcp
```

Run `/mcp` in Claude Code, pick `radar` and sign in.

**claude.ai**: Settings → Connectors → Add custom connector, name `Radar`,
URL `https://radar-mcp.example.com/mcp`, then Connect and sign in.

**Tools**

| Tool | Does |
| --- | --- |
| `radar_review(repo, sha)` | Returns the stored review of the commit, or starts one and waits up to 45 seconds. A review takes several minutes, so it usually answers `running`. |
| `radar_status(repo, sha)` | Returns the stored review, or `running`, `failed` (try again), `rejected` or `not_found`. Starts nothing. |

`repo` is `owner/name`; `sha` is a full or abbreviated SHA of a commit already
pushed to GitHub. The review is the one CI would post on a pull request from
that commit into the default branch: the same reviewers, policy and guidance
(read from the default branch's tip, never from the commit), review packs and
rendered Markdown. A repository with no `.github/radar-policy.json` of its own
is reviewed under Radar's default policy (`policy_source: radar-default` in
the result; `radar-gh review-commit -default-policy` in the README).
Pull-request-only gates (checks, approvals, unresolved threads) are skipped;
see `radar-gh review-commit` in the README. A commit Radar cannot review (no
changes against the default branch, or 300 or more files) is `rejected`:
final for that commit and configuration, and asking again costs nothing.

A review is stored by repository, commit, Radar version and reviewer
configuration, so asking again returns it instantly and anyone can read it
with `radar_status`. A new Radar version or a reviewer change reviews afresh.
Only a complete review is stored: if any reviewer fails (a provider error or
throttle, a timeout, an answer it could not parse), the review is `failed`,
naming the reviewer and why, nothing is stored, and the next `radar_review`
runs it again, as CI records no failed review.

Limits (configurable): new reviews an hour and running reviews per person,
counted on each replica, and one running review per replica (see Codex
isolation). Scale replicas on `radar_mcp_reviews_in_flight`. When every
replica is busy, `radar_review` answers that the server is busy; try again in
a few minutes. Stored reviews and `radar_status` are free.

## Design

```
                     ┌──────────────────────────── pod radar-mcp ───────────────────────────┐
 claude.ai /         │ auth proxy            server container            reviewer container  │
 Claude Code ──TLS──▶│ :8090  OAuth ────────▶ radar-mcp serve            radar-mcp runner    │
   (Ingress)         │ your domain only       127.0.0.1:8080 (MCP)        uid 65533           │
                     │ sets X-Radar-MCP-      127.0.0.1:8081 (broker)     radar-gh, Codex     │
                     │ Email / Subject        uid 65532                   no credentials      │
                     │                        IRSA token, App key ──────▶ /run/radar-runner/  │
                     │                        S3 results          socket  runner.sock         │
                     └──────────────────────────────────────────────────────────────────────┘
```

1. An **OAuth proxy** in front of the server terminates sign-in, admits only
   your organization's accounts, strips caller-supplied identity headers and
   sets `X-Radar-MCP-Email` and `X-Radar-MCP-Subject` from the verified token.
   It also adds `X-Radar-MCP-Proxy-Token`, a secret only it and the server
   container hold, and the server refuses any request without exactly that
   value: the MCP listener is on pod loopback, which the reviewer container
   shares, so the identity headers alone could be forged from inside the pod.
   The Service publishes only the proxy.
2. The **server container** checks the repository against the allowlist,
   resolves the SHA through the GitHub App, and looks up the result in S3. To
   start a review it claims it with a conditional write (`If-None-Match`, or
   `If-Match` over a failed or stale marker), so two replicas never run the
   same review. A replica that dies leaves a `running` marker that goes stale
   after the review timeout plus five minutes.
3. It sends the review to the **reviewer container** over a Unix socket, with
   the review's only credentials: a GitHub installation token limited to that
   one repository's contents (read), and a URL and per-review bearer token for
   the credentials broker.
4. The runner runs `radar-gh review-commit -json`, the CLI CI's code path
   uses, and returns its JSON. The server checks it is the commit asked for
   and that no reviewer in it failed safe (radar-gh exits 0 when one does),
   and stores it; otherwise it records a `failed` marker, which the next
   `radar_review` claims and runs again.

Configuration is a JSON file (`examples/radar-mcp-config.json`): the
repository allowlist and, per repository, the reviewers and Radar settings
its CI caller workflow passes. Only repositories every engineer can already
read belong on the allowlist: any signed-in user can review any of them, and
findings quote code.

**Every repository of an organization.** An `"owner/*"` entry admits every
repository in the organization. An exact entry (`owner/repo`) overrides it
for that repository. Use the wildcard only when the organization's base
permission is `read`, so every member can read every repository; the server
does not check that, so remove the entry if the base permission is ever
lowered (DECISIONS.md). Before a review the wildcard admits, the server
checks the following and refuses the review when any fails:

- **Same organization.** GitHub must name the repository in that same
  organization, and the App must be able to read it. A transferred
  repository's old name redirects elsewhere and is refused.
- **Not archived.** Nothing new can be pushed to an archived repository.

The repository is then reviewed under GitHub's spelling of its name, with the
wildcard entry's reviewers and Radar's default policy when it has none of its
own. A wildcard entry must name at least two reviewers, since the default
policy requires every reviewer to pass.

## Codex isolation

Codex reviews by running commands over a checkout of the commit. It runs as
its container's user in a read-only sandbox, so it can read any file that
user can, and it is steered by text in the repository under review, which is
untrusted. In CI that is safe because the job's credentials live only in
process environments and memory, which radar-gh keeps from Codex. In a pod,
the IRSA web identity token is a file; a Codex that read it could assume the
pod's role. So Codex must not be able to reach the IRSA token, the results
bucket or the GitHub App key.

**Separate container.** Codex runs only in the reviewer container, which has
its own filesystem and, the pod not sharing a process namespace, its own
processes. The reviewer container holds nothing worth reading:

| Secret | Where it is | How the reviewer is kept from it |
| --- | --- | --- |
| IRSA web identity token | server container only | `eks.amazonaws.com/skip-containers: reviewer,auth-proxy` stops the pod identity webhook projecting it or its environment into the reviewer (and the internet-facing auth proxy). |
| Results bucket access | server container (IRSA role) | The reviewer has no AWS identity of its own. The credentials it is lent are scoped by a session policy to model inference only (below). |
| GitHub App private key | server container only | Mounted from its Secret into the server container alone. The reviewer gets only an installation token for one repository's contents, read-only, expiring within the hour. |
| Kubernetes service account token | nowhere | `automountServiceAccountToken: false`. |
| Node credentials (IMDS) | node | Set the nodes' IMDSv2 hop limit and have the pod's NetworkPolicy refuse egress to 169.254.169.254; radar-gh runs with `AWS_EC2_METADATA_DISABLED`. |

**Credentials broker.** radar-gh still needs AWS credentials to call
Bedrock. The server container assumes its IRSA role again from the web
identity token with a session policy that allows only
`bedrock:InvokeModel*`, `bedrock-mantle:CreateInference` and the Bedrock
Marketplace subscribe actions (`reviewSessionPolicy`, `cmd/radar-mcp/broker.go`).
AWS intersects a session policy with the role's, so these credentials can call
the models the role allows and nothing else: not the results bucket. It serves
them on pod loopback in the AWS SDKs' container-credentials format, and only
to the bearer token issued for a running review, revoked when it ends.
radar-gh finds them through `AWS_CONTAINER_CREDENTIALS_FULL_URI`.

**Inside the reviewer**, Codex sees still less. The runner builds radar-gh's
environment from the request alone, never its own. radar-gh passes Codex only
`PATH`, `HOME`, its config and a placeholder AWS key: Codex reaches Bedrock
through radar-gh's loopback signing proxy and never holds a credential. The
runner, and radar-gh whenever it runs Codex, mark themselves non-dumpable, so
their environment and memory (the GitHub token, the broker token) are
unreadable from `/proc` to Codex though it runs as the same user. Codex's own
sandbox (bubblewrap) runs each command read-only and without network.

That sandbox needs a user namespace, which the container runtime's default
seccomp profile forbids; there every Codex command fails and Codex reviews
the diff alone, unlike CI; an AppArmor profile like Docker's default also
forbids the mounts it makes. The reviewer container therefore runs with
`seccompProfile: Unconfined` and `appArmorProfile: Unconfined`, keeping
`runAsNonRoot`, `readOnlyRootFilesystem`, `allowPrivilegeEscalation: false`
and all capabilities dropped. This loosens only the container that holds
nothing. Nodes must also allow unprivileged user namespaces: AL2023 does by
default; Ubuntu 24.04 forbids them through AppArmor
(`kernel.apparmor_restrict_unprivileged_userns`), as on GitHub's runners.
Either way the runner refuses to start where the sandbox cannot, so the
failure is a reviewer that is not ready, never a review made without reading
the code.

**One review per reviewer container.** Non-dumpable is reset on `execve`,
so the `gh` and `git` processes radar-gh starts while fetching carry the
review's GitHub and broker tokens in a readable `/proc/<pid>/environ`. They
finish before Codex starts, so a review's Codex cannot catch its own; but a
second review running alongside could catch them. The runner therefore runs
one review at a time, the server admits one per replica, and the
configuration refuses more. Capacity comes from replicas.

**What remains.** The pod shares one network namespace, so a process in the
reviewer could reach the server's loopback listeners. Codex's sandbox gives
its commands no network; the broker answers only a live review's bearer
token, which only radar-gh's non-dumpable memory holds; and the MCP listener
requires the proxy's secret, which the reviewer does not have. Were Codex to
obtain its own review's broker token anyway, it would get Bedrock-only
credentials for the minutes the review lasts, what radar-gh already uses on
its behalf.

**Proof.**

- The runner checks its own isolation before serving and refuses to start if
  the reviewer container can read the IRSA, pod identity or service account
  token, the App key (`RADAR_MCP_FORBIDDEN_PATHS`) or an AWS or gh
  credentials file under `$HOME`, carries a credential or server variable
  (including the proxy secret, the AWS SDKs' credential-file variables and
  model API keys), or reaches the instance metadata service. It also refuses
  when Codex's sandbox cannot start. The same check is the reviewer's
  readiness probe (`radar-mcp check-isolation`), so a deployment change that
  mounts a secret into the reviewer takes the pod out of service.
- `scripts/test-mcp-isolation.sh IMAGE` exercises a built image: the check
  passes as deployed; it fails, and the runner refuses to start, with the IRSA
  token, the App key, the IRSA environment or the bucket name in the
  reviewer; a reviewer process cannot read the token; Codex's sandbox reads
  but cannot write or reach the network; and the runner refuses under the
  default seccomp profile. Run it on every image build.
- Unit tests: radar-gh's environment is exactly the request's
  (`TestReviewEnvIsOnlyTheRequest`, `TestRunnerRunsRadarGHWithTheReviewsCredentialsOnly`);
  the session policy grants only model inference
  (`TestReviewSessionPolicyGrantsOnlyModelInference`); the broker serves only
  a live review token and the SDK's default chain reads it
  (`TestBrokerServesOnlyALiveReviewToken`, `TestTheSDKReadsBrokerCredentials`);
  App tokens are one repository, contents and metadata read, and fresh per
  review (`TestGitHubAppMintsOneRepositoryReadOnlyTokens`); requests without
  the proxy's secret are refused, even with identity headers
  (`TestUnauthenticatedRequestsAreRefused`).

## Edge and timeouts

Whatever sits in front of the proxy (a CDN, WAF or load balancer) must let
the MCP OAuth flow through: no interactive login wall such as an access
gateway, no source-IP allowlist (claude.ai calls from Anthropic's servers,
Claude Code from wherever the engineer is) and no browser challenges, since
every client is programmatic. The OAuth proxy stays the identity gate. If
you do put a CDN in front, have the origin accept only that CDN (for example
with authenticated origin pulls using your own client certificate), restrict
paths to those the proxy serves, rate-limit the OAuth endpoints per source,
and bypass caching for the host.

The server runs the MCP Streamable HTTP transport stateless with JSON
responses: every call is one `POST` and one JSON response, and a `GET` for a
standalone event stream is answered 405, so no long-lived stream crosses the
edge. The longest call is `radar_review`, which answers within `review_wait`
(45s by default) plus a GitHub lookup. What bounds it in practice is the MCP
client: Claude Code and the claude.ai connector time out long tool calls,
and a review takes minutes, so `radar_review` answers "running" and
`radar_status` polling carries the rest. No hop in front of the server may
cut a call shorter than the configured wait: derive the ingress read and send
timeouts from `review_wait` (for example `review_wait` plus 60s, at least
120s) so the two cannot drift.

The server records each review's end-to-end duration by reviewer
configuration and result (`radar_mcp_review_duration_seconds{agent,result}`,
and `agent`, `result` and `duration` in the `radar review` log line), and
each tool call's latency (`radar_mcp_tool_call_duration_seconds{tool}`), so a
ceiling for `review_wait` can be chosen from data.

## Deploying (Kubernetes)

- **Pod**: annotation `eks.amazonaws.com/skip-containers: reviewer,auth-proxy`;
  `automountServiceAccountToken: false`; ServiceAccount with the IRSA role
  annotation; `terminationGracePeriodSeconds` longer than
  `RADAR_MCP_DRAIN_TIMEOUT` (default 20m) plus a minute.
- **auth proxy**: any OAuth proxy that supports the MCP authorization flow,
  forwards to `http://localhost:8080`, and sets the identity headers above
  plus `X-Radar-MCP-Proxy-Token:<token>` from a generated Secret (at least
  32 random alphanumeric characters).
- **server** (`radar-mcp serve`, uid/gid 65532): env
  `RADAR_MCP_PROXY_TOKEN` (the same Secret), `RADAR_MCP_RESULTS_BUCKET`,
  `RADAR_MCP_GITHUB_APP_ID`,
  `RADAR_MCP_GITHUB_APP_KEY_FILE=/etc/radar-mcp/github-app/private-key.pem`,
  `POD_NAME`; mounts the config at `/etc/radar-mcp/config.json`, the App key
  Secret at `/etc/radar-mcp/github-app/`, and the socket volume at
  `/run/radar-runner`. Readiness `/readyz` on `:9090` (checks the runner).
- **reviewer** (`radar-mcp runner`, uid/gid 65533): no Secret mounts and no
  credential env; `RADAR_MCP_FORBIDDEN_PATHS=/etc/radar-mcp/github-app/private-key.pem`;
  `RADAR_MCP_WORK_DIR=/work`; the socket volume at `/run/radar-runner`; an
  `emptyDir` at `/work` sized for one review's two checkouts of your largest
  repository and a small `emptyDir` at `/tmp` (Codex keeps
  `/tmp/codex-daemon-<uid>` there whatever `TMPDIR` says; without it the
  runner refuses to start), the only writable places;
  `seccompProfile: Unconfined` and `appArmorProfile: Unconfined` (above);
  readiness exec `["/usr/local/bin/radar-mcp", "check-isolation"]`.
- **IRSA role**: Bedrock model invocation plus `s3:GetObject`,
  `s3:PutObject`, `s3:DeleteObject` on the results bucket's `reviews/*`, and
  `s3:ListBucket` on the bucket, unconditioned (without it S3 answers a
  missing review with 403, not 404; an `s3:prefix` condition would too, since
  a GetObject carries no prefix, and the bucket holds only reviews). Trust:
  the `radar-mcp` ServiceAccount.
- **Results bucket**: private, encrypted, a lifecycle rule expiring
  `reviews/` after 90 days.
- **Image**: `docker build -f Dockerfile.mcp .`. Publish it only from the
  default branch: tags are unprotected and could name unreviewed code.
- **NetworkPolicy**: ingress to 8090 from your ingress controller and to 9090
  from your metrics scraper. The pod needs egress 443 to GitHub, Bedrock,
  Bedrock Mantle, STS and S3.

Rollback is removing the deployment; nothing else depends on it, and stored
reviews stay in the bucket.

## GitHub App

1. **Create the App** in your organization's settings → GitHub Apps → New:
   - Webhook: clear **Active**.
   - Repository permissions: **Contents: Read-only**. Metadata becomes
     Read-only automatically. Leave everything else **No access**, and all
     organization and account permissions.
   - Where can this GitHub App be installed: **Only on this account**.
   - Note the **App ID**, then **Generate a private key**.
2. **Install it** on the repositories you allowlist, or on all repositories:
   the App can only read contents, each review's token is scoped to its one
   repository, and the allowlist in the configuration decides what can be
   reviewed.
3. **Store the App ID and private key** in your secret store and project them
   into a Secret mounted only into the server container (keys `app-id`,
   `private-key.pem`); then delete the downloaded key file.

## Configuration reference

`radar-mcp serve`:

| Variable | Default | |
| --- | --- | --- |
| `RADAR_MCP_CONFIG` | `/etc/radar-mcp/config.json` | Allowlist, reviewers, limits. |
| `RADAR_MCP_PROXY_TOKEN` | required | The secret the auth proxy sends in `X-Radar-MCP-Proxy-Token`; 32+ characters. |
| `RADAR_MCP_RESULTS_BUCKET` | required | |
| `RADAR_MCP_RESULTS_PREFIX` | `reviews/v1` | |
| `RADAR_MCP_GITHUB_APP_ID`, `RADAR_MCP_GITHUB_APP_KEY_FILE` | required | |
| `RADAR_MCP_REVIEW_PACKS` | `/opt/radar/review-packs` | Hashed into the configuration digest. |
| `RADAR_MCP_RUNNER_SOCKET` | `/run/radar-runner/runner.sock` | |
| `RADAR_MCP_LISTEN_ADDRESS` | `127.0.0.1:8080` | MCP, behind the auth proxy. |
| `RADAR_MCP_BROKER_LISTEN_ADDRESS` | `127.0.0.1:8081` | Must be loopback. |
| `RADAR_MCP_METRICS_LISTEN_ADDRESS` | `:9090` | `/metrics`, `/healthz`, `/readyz`. |
| `RADAR_MCP_DRAIN_TIMEOUT` | `20m` | On shutdown, new reviews are refused and running ones may finish this long before they are cancelled and recorded as failed. |

`radar-mcp runner`: `RADAR_MCP_RUNNER_SOCKET`, `RADAR_MCP_WORK_DIR`,
`RADAR_MCP_REVIEW_PACKS`, `RADAR_MCP_RADAR_GH` (default `radar-gh` on
`PATH`), `RADAR_MCP_FORBIDDEN_PATHS` (comma-separated, added to the token
paths the isolation check always covers).

Metrics: `radar_mcp_tool_calls_total{tool,result}`,
`radar_mcp_tool_call_duration_seconds`, `radar_mcp_review_admissions_total{result}`,
`radar_mcp_reviews_total{result}`, `radar_mcp_review_duration_seconds{agent,result}`,
`radar_mcp_reviews_in_flight`. Logs name the tool, repository, commit,
outcome and the caller's email; never findings, tokens or credentials.

Build the image locally with your review packs in `review-packs/` and
`docker build -f Dockerfile.mcp .`, then `scripts/test-mcp-isolation.sh IMAGE`.
