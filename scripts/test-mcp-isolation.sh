#!/usr/bin/env bash
# Proves, on the built image, that the reviewer container's isolation check
# passes only when the container holds no credential: it passes as deployed
# (no token, no key, no credential environment) and fails, and the runner
# refuses to start, when the IRSA token, the GitHub App key or a credential
# variable reaches it. CI runs this on every image (radar-mcp-image.yml).
#
#   scripts/test-mcp-isolation.sh IMAGE
set -euo pipefail

image="${1:?usage: $0 IMAGE}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
printf 'eyJhbGciOiJSUzI1NiJ9.fake.irsa' >"$tmp/token"
printf -- '-----BEGIN RSA PRIVATE KEY-----\nfake\n' >"$tmp/key.pem"
chmod 0644 "$tmp/token" "$tmp/key.pem"

irsa=/var/run/secrets/eks.amazonaws.com/serviceaccount/token
appkey=/etc/radar-mcp/github-app/private-key.pem
# The reviewer container as the chart runs it: its own user, read-only root,
# writable only the work volume and /tmp (Codex keeps /tmp/codex-daemon-<uid>
# there whatever TMPDIR says), no capabilities, no privilege escalation, and
# the unconfined seccomp and AppArmor profiles Codex's sandbox needs to create
# its namespaces and mounts (docs/mcp.md).
work=(--tmpfs /work:mode=1777 -e RADAR_MCP_WORK_DIR=/work)
reviewer=(docker run --rm --user 65533:65533 --read-only "${work[@]}" --tmpfs /tmp:mode=1777
  --cap-drop ALL --security-opt no-new-privileges
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined
  -e RADAR_MCP_FORBIDDEN_PATHS="$appkey")
fail=0

expect() {
  local want="$1" name="$2"
  shift 2
  set +e
  out="$("$@" 2>&1)"
  got=$?
  set -e
  # exit1: the runner must refuse (exit 1), not hang until the timeout.
  if [[ "$want" == pass && "$got" -ne 0 ]] || [[ "$want" == fail && "$got" -eq 0 ]] || [[ "$want" == exit1 && "$got" -ne 1 ]]; then
    echo "FAIL: $name (exit $got): $out"
    fail=1
  else
    echo "ok:   $name"
  fi
}

expect pass "reviewer as deployed is isolated" \
  "${reviewer[@]}" "$image" check-isolation
expect fail "IRSA token mounted into the reviewer" \
  "${reviewer[@]}" -v "$tmp/token:$irsa:ro" "$image" check-isolation
expect fail "GitHub App key mounted into the reviewer" \
  "${reviewer[@]}" -v "$tmp/key.pem:$appkey:ro" "$image" check-isolation
expect fail "IRSA environment in the reviewer" \
  "${reviewer[@]}" -e AWS_ROLE_ARN=arn:aws:iam::123456789012:role/radar-mcp -e AWS_WEB_IDENTITY_TOKEN_FILE="$irsa" "$image" check-isolation
expect fail "AWS credentials file mounted elsewhere and named by the SDK variable" \
  "${reviewer[@]}" -v "$tmp/token:/mnt/creds:ro" -e AWS_SHARED_CREDENTIALS_FILE=/mnt/creds "$image" check-isolation
expect fail "results bucket named in the reviewer" \
  "${reviewer[@]}" -e RADAR_MCP_RESULTS_BUCKET=radar-mcp-results "$image" check-isolation
expect exit1 "runner refuses to start with the IRSA token" \
  timeout 60 "${reviewer[@]}" -v "$tmp/token:$irsa:ro" "$image" runner

# What Codex would do: read the token as the reviewer user. With the token
# only in the server container, there is nothing to read.
expect fail "a reviewer process cannot read the IRSA token" \
  "${reviewer[@]}" --entrypoint cat "$image" "$irsa"

# Codex's own sandbox works in the reviewer as deployed: it reads the
# checkout, cannot write it and cannot reach the network, as in CI.
sandbox='mkdir -p /work/c /work/repo && echo hi >/work/repo/f && cd /work/repo &&
  read="$(codex sandbox -- cat f 2>&1)" && [ "$(printf %s "$read" | tail -n 1)" = hi ] || { echo "$read"; exit 1; }
  
  ! codex sandbox -- sh -c "echo x > g" 2>/dev/null &&
  ! codex sandbox -- sh -c "getent hosts github.com" 2>/dev/null'
expect pass "Codex sandbox reads, cannot write or reach the network" \
  "${reviewer[@]}" -e HOME=/work -e CODEX_HOME=/work/c -e TMPDIR=/work --entrypoint sh "$image" -c "$sandbox"
# Under the runtime's default seccomp profile the sandbox cannot start, so
# the runner refuses rather than let Codex review without reading the code.
expect exit1 "runner refuses when the Codex sandbox cannot start" \
  timeout 60 docker run --rm --user 65533:65533 --read-only "${work[@]}" --tmpfs /tmp:mode=1777 --cap-drop ALL "$image" runner
# Without a writable /tmp the sandbox cannot start either; the runner refuses.
expect exit1 "runner refuses without a writable /tmp" \
  timeout 60 docker run --rm --user 65533:65533 --read-only "${work[@]}" --cap-drop ALL --security-opt no-new-privileges \
  --security-opt seccomp=unconfined --security-opt apparmor=unconfined "$image" runner

# And as deployed, the runner passes both checks and serves.
runner_starts() {
  local id
  id="$("${reviewer[@]}" -d --tmpfs /run/radar-runner:mode=1777 "$image" runner)"
  for _ in $(seq 30); do
    if docker logs "$id" 2>&1 | grep -q 'runner ready'; then
      docker rm -f "$id" >/dev/null
      return 0
    fi
    sleep 1
  done
  docker logs "$id" 2>&1 | tail -5
  docker rm -f "$id" >/dev/null
  return 1
}
expect pass "runner starts as deployed" runner_starts

exit "$fail"
