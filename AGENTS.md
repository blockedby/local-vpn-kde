# Local VPN agent notes

## Validation and runtime

- Prefer local unit, functional, and integration checks. Run the Go race tests (`go test -race ./...`) and the OpenTUI checks (`bun run check` and `bun test` in `scripts/vpnkit/tui`) for a release candidate.
- Validate installer, routing, gateway, or connectivity changes in disposable Docker/Podman containers using existing `test/` runners. Preserve the host VPN, resolver, routes, and production containers. A host smoke script is not a substitute for an isolated lab.
- Record the tested commit, commands, outcomes, and material skips in the related GitHub issue. Close an issue only after its change and relevant acceptance checks are complete. Keep commits atomic.
- Never commit subscriptions, private endpoints, keys, generated profiles, or private logs. Pass real lab subscriptions through the supported environment variable; redact evidence.

## Stable versions and GitHub releases

- Only a human can approve a revision as stable. Under the explicitly authorized release process, a maintainer merging a PR into `main` with the `release` label and a changed `VERSION` approves the exact merged revision and its release workflow. An ordinary merge or an agent's judgment does not grant approval. Agents must not label or merge a release PR without explicit release authorization.
- Stable version tags are immutable. Never force-update or reuse one for a different commit; corrections get a new version.
- Use the `release-vpn` skill when preparing or publishing a release. If it is unavailable, follow these rules and report that limitation.
- Run routine checks locally. The release workflow may run only on `pull_request: closed` targeting `main`, gated on merged=true and the `release` label. It reads `VERSION` from the exact merge SHA, checks and builds before creating the immutable tag. Ordinary PRs have no build jobs; pushes, tag pushes and schedules must not trigger CI. The native candidate workflow remains manual (`workflow_dispatch`).
- A human-approved release-labelled PR merge authorizes its configured CI run and publication. Creating/pushing a tag alone does not authorize or trigger CI. Manual candidate runs and retries still require explicit authorization; never automatically retry failed runs.
- Before publication, verify all assets and source manifests belong to the approved merged commit. Create the tag only after checks/builds pass; verify it resolves to that commit. Upload as a draft, verify downloaded SHA-256 checksums, then make it public. Never replace existing tags/releases/assets.
- This application includes Go code, a Bun/OpenTUI interface, container configuration, and installer assets. Do not present a Go binary alone as the complete application. Describe the contents and remaining installation dependencies of the release archive accurately.
- Do not deploy or reconnect the live VPN as part of tagging or publishing a release.
