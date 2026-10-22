---
name: release-vpn
description: Prepare or publish a human-approved release of blockedby/local-vpn-kde through a release-labelled PR, VERSION and verified package assets.
---

# Release local-vpn-kde

Work in the checkout whose Git origin is `blockedby/local-vpn-kde`; the usual local path is `~/code/tools/local-vpn-kde`. Read its `AGENTS.md`. Do not use the former `vibe-practicum-vpn` repository.

## Authorization and scope

The user-selected release process is a PR into `main` with the `release` label
and an explicit version in the root `VERSION` file. A maintainer's merge of that
PR approves the exact merged revision and authorizes the configured CI build and
publication. An ordinary merge, a candidate build, or an agent's judgment is not
stable approval. Agents may prepare changes but must not add the label or merge
a release PR without explicit release authorization.

- Stable versions use `MAJOR.MINOR.PATCH` in `VERSION`, without `v`. Tags add `v`.
- Never move, reuse or overwrite stable tags, releases or existing release assets.
- Release CI runs only on a merged, release-labelled PR into `main`; ordinary PRs
  do not run build jobs, and pushes/tag pushes/schedules trigger nothing.
- The native candidate workflow remains manual. Manual runs and retries require
  explicit authorization; a failed run must be inspected, not automatically retried.
- Release work never authorizes changing a live VPN or system routes.

## Prepare a release PR

1. Read `AGENTS.md`, inspect status and existing tags, and use the isolated feature
   worktree/PR workflow. Preserve unrelated edits.
2. Update `VERSION` in the PR. Choose an unused stable version greater than all
   existing stable tags; do not infer stable approval from successful tests.
3. Validate locally: `go test -race ./...`, build the Go bridge, then locked Bun
   dependencies, `bun run check` and `bun test` in `scripts/vpnkit/tui` for a release
   candidate. Validate changed installer/network behavior in disposable labs.
   For automation-only changes, run the release control-plane tests,
   `test/release-package-test.sh`, actionlint and relevant packaging tests.
4. Record the exact tested commit, commands, outcomes and material skips in the
   related issue. Never include subscriptions, keys or private logs in evidence.
5. Confirm platform acceptance and redistribution requirements before requesting
   stable approval. Native container checks do not prove KDE/Polkit/live VPN
   acceptance on a new platform.

## Authorized publication

1. A human-authorized merge of the release-labelled PR starts `release.yml`.
   Do not additionally dispatch or manually push a tag.
2. The workflow reads `VERSION` and packages the exact `merge_commit_sha`. Go/TUI
   checks and isolated Arch/Debian package checks run before the publish job.
   Docker installer packaging uses the exact SHA without creating a tag early.
3. The publish job verifies matrix checksums and provenance, refuses existing or
   superseded versions, creates an immutable annotated tag, and verifies its SHA.
4. Assets are uploaded to a draft release and downloaded again for checksum
   verification before the draft becomes public. Report the resulting URL,
   version, commit, contents and validation limitations.
5. If publication fails after tag creation, preserve the immutable tag and any
   draft. Do not retry or overwrite automatically. Inspect the failure and obtain
   explicit authorization for recovery; a new revision requires a new version.

The release contains native Arch/Debian packages with compiled OpenTUI, Go
services and sing-box, plus a separate Docker installer archive. Native still
requires system networking, NetworkManager/OpenVPN, systemd and polkit. The Docker
archive installs dependencies and rebuilds the app. Do not describe a Go binary
alone as the complete application. Include source provenance and SHA-256 checksums.
