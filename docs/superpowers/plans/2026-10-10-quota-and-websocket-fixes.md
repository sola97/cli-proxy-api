# Quota and WebSocket Fixes Implementation Plan

> Execute in the current chat. The primary agent owns integration, validation, commit, push and deployment; workers own only the assigned files.

**Goal:** Fix the Codex quota 404 in issue #6504 and preserve confirmed WebSocket quota cooldown in issue #6503. The user explicitly excluded Grok 426 from this task.

**Architecture:** Register the existing quota handlers under the authenticated v8 management group. Preserve scheduler recording of confirmed upstream quota errors when downstream disconnect cancels the request.

**Tech Stack:** Go 1.26, Gin, Docker in WSL Ubuntu-22.04; existing C ABI quota plugin.

**Spec:** User request in this chat; upstream issues https://github.com/router-for-me/CLIProxyAPI/issues/6504 and https://github.com/router-for-me/CLIProxyAPI/issues/6503.

## Constraints

- Base all changes on v8.0.23 / d318bcc3.
- Push to sola97/cli-proxy-api using fix(scope) commits.
- Build in WSL or GitHub Actions, never on bandwagon.
- Preserve management authentication, disabled/home mode behavior and unrelated API-call headers.
- Pure client cancellation and transport EOF must not produce quota cooldown.
- Retain current production config, credentials and plugin; back up deployment and retain rollback image.

## Task 1: v8 quota contract

Files: internal/api/server_management_v8.go; internal/api/server_management_v8_quota_test.go.

- [ ] Add a route-level fixture implementing QuotaProvider; assert provider listing, normalized quota fetch, reset, validation and access control through /v8/management/quota/*.
- [ ] Run `go test ./internal/api -run TestManagementV8Quota -count=1`; confirm the unpatched route fails with 404.
- [ ] Register `v8.GET("/quota/providers", s.mgmt.GetQuotaProviders)`, `v8.POST("/quota/fetch", s.mgmt.FetchCredentialQuota)`, `v8.POST("/quota/reset", s.mgmt.ResetCredentialQuota)`.
- [ ] Rerun route tests and existing v8 contract tests; commit and push with `fix(management): register v8 credential quota operations`.

## Task 2: confirmed WebSocket quota cooldown

Files: sdk/cliproxy/auth/conductor_stream.go and targeted stream cancellation tests; executor WebSocket error handling only if required.

- [ ] Reproduce canceled context before confirmed error recording and assert credential cooldown plus subsequent selection exclusion.
- [ ] Cover ordinary cancellation/EOF, model-level cooling and healthy credential routing.
- [ ] Run focused conductor/executor regressions and the synthetic mock reproduction from #6503 without production credentials.
- [ ] Commit and push with `fix(codex): preserve websocket quota cooldown after disconnect`.

## Task 3: build and deployment

- [ ] Run affected packages and compile cmd/server with CGO enabled in the WSL Go 1.26 builder.
- [ ] Review final diff and run independent read-only reviewer.
- [ ] Build a versioned amd64 image; load it on bandwagon and switch compose only after backup.
- [ ] Verify version, quota plugin load, HTTP models and authenticated real quota refresh for active Codex credentials.
- [ ] Record artifact identifiers, commands, results and limitations in a deployment report; commit and push docs.
