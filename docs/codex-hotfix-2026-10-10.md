# Codex quota and WebSocket hotfix

This fork fixes the Codex quota management 404 from upstream issue [6504](https://github.com/router-for-me/CLIProxyAPI/issues/6504) and the confirmed WebSocket quota cooldown cancellation race from [6503](https://github.com/router-for-me/CLIProxyAPI/issues/6503). It is based on v8.0.23 (`d318bcc3`). Grok 426 was explicitly excluded.

## Changes

- `037414f`: register the existing quota provider, fetch and reset handlers under the authenticated `/v8/management/quota` group. Existing authentication, management availability and Home-mode behavior remain covered by route tests.
- `c74e03c`: retain confirmed `usage_limit_reached` errors with HTTP 429 and a retry interval through WebSocket disconnect and request cancellation. Handle both `type:error` and `response.failed`; preserve credential/model scope, and drain internal quota results without holding canceled Codex output open. Preserve normal stream tail processing and the existing receive behavior of other providers.
- `74e3354`: add GitHub Actions tests, compilation and amd64 image publication.

## Validation

The final source passed:

```sh
go test ./internal/api ./internal/api/handlers/management ./sdk/cliproxy/auth ./internal/runtime/executor -count=1
go test -race ./sdk/cliproxy/auth ./internal/runtime/executor -run Quota -count=20
CGO_ENABLED=1 GOOS=linux go build -buildvcs=false -o /tmp/CLIProxyAPI-final-check ./cmd/server
```

The existing Claude tail-cancellation regression passed 100 repetitions. New tests cover confirmed quota versus cancellation/EOF, credential/model cooldown scope, subsequent healthy credential selection, bootstrap and post-payload cancellation, source channels that remain open, and reader-confirmed quota delivery after cancellation. Independent read-only review found no remaining blocking issues.

The new confirmed-quota cancellation regression fails against the unmodified v8.0.23 source: the canceled request leaves its exhausted credential selectable. It passes with this fix.

Both the WSL-built image and the final GitHub Actions image passed isolated Docker tests with synthetic credentials and `--network none`. For each of `type:error` and `response.failed` with a 600-second quota reset, three new downstream WebSocket connections yielded cumulative upstream request counts of `1, 1, 1`. A subsequent HTTP request returned 429 without another upstream call. The original issue's timing-sensitive mock did not reproduce repeated calls on the unmodified image in this WSL environment; the deterministic cancellation regression provides the before/after failure evidence.

A full repository suite was not run. One earlier affected-package run encountered an intermittent keepalive upload test failure; its original version passed 100 focused repetitions, and the final affected-package run and GitHub Actions run both passed. No unrelated keepalive code was changed. The pending-reader regression uses a real local WebSocket and production reader/delivery functions; it does not individually execute every cancellation exit end to end. Code review checked those shared-function call sites.

## Artifact and deployment

- Source branch: [`shenlp/fix-v8-quota-routes`](https://github.com/sola97/cli-proxy-api/tree/shenlp/fix-v8-quota-routes).
- Build: [GitHub Actions run 38042057441](https://github.com/sola97/cli-proxy-api/actions/runs/38042057441), completed successfully against `c74e03c2e50b1b6f30ee67083806c013a8e47ec2`.
- Image tag: `ghcr.io/sola97/cli-proxy-api:v8.0.23-codex-fix`.
- Commit tag: `ghcr.io/sola97/cli-proxy-api:sha-c74e03c2e50b1b6f30ee67083806c013a8e47ec2`.
- Deployed immutable image: `ghcr.io/sola97/cli-proxy-api@sha256:2c45b5504295505333417602499cfd244f852f66e4910f1a01925678b72f19d5`.
- Runtime: Linux amd64, `v8.0.23-sola97-codex-fix`, commit `c74e03c`, built `2026-10-10T09:38:40Z`.

The image was built in GitHub Actions and independently compiled in WSL, never on bandwagon. The bandwagon Compose image was pinned to the digest above. Configuration, credentials and the existing C ABI quota plugin v0.3.2 were retained. The container started with restart count zero; the plugin registered successfully.

Before deployment, the Compose file was backed up at `/root/docker-cpa/docker-compose.yml.before-codex-fix-20261010-094238`; the original pinned v8.0.23 image remains available. To roll back on bandwagon:

```sh
cd /root/docker-cpa
cp docker-compose.yml.before-codex-fix-20261010-094238 docker-compose.yml
docker compose up -d --no-deps --force-recreate cli-proxy-api
```

## Production verification

The local DNS entry for `cpa.sora.vip` points at another server. Verification targeted bandwagon directly over SSH and, for the public domain, through `socks5h://localhost:1083`.

Before the hotfix, authenticated requests to the v8 quota provider/fetch routes returned 404. The existing plugin fetch operation initially returned an empty cache. Triggering the plugin's existing asynchronous refresh populated the three enabled Codex accounts, proving there was no quota response schema mismatch. This plugin operation reads cached quota; after a restart, its default configuration does not immediately refresh that cache, so the existing refresh endpoint was triggered again for verification.

After deployment, `/v8/management/quota/providers` returns the registered Codex provider. All three enabled accounts return HTTP 200 from `/v8/management/quota/fetch`, with a Plus subscription and two nonempty quota groups. The disabled account has an existing invalid refresh token and was excluded from active-account verification. Management credentials and account identifiers are omitted from this report.

A browser launched through the SOCKS5 proxy logged into the deployed management page, opened the credential list and clicked the actual "refresh quota" button. The page's `/v8/management/quota/fetch` request returned 200 with two groups; the UI displayed a successful refresh message and remaining percentages of 95% and 52% for the selected account. The dashboard displayed the hotfix version and a model catalog with 15 entries.

Production currently sets `routing.cooldown.disable-cooling: true`. This setting was preserved; core scheduler cooldown behavior was verified with cooling enabled in the isolated tests. Enabling the production core cooldown is a separate configuration decision.
