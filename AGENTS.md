# Local VPN agent notes

## Validation and runtime

- Prefer local unit, functional, and integration checks. Run the Go race tests (`go test -race ./...`) and the OpenTUI checks (`bun run check` and `bun test` in `scripts/vpnkit/tui`) for a release candidate.
- Validate installer, routing, gateway, or connectivity changes in disposable Docker/Podman containers using existing `test/` runners. Preserve the host VPN, resolver, routes, and production containers. A host smoke script is not a substitute for an isolated lab.
- Record the tested commit, commands, outcomes, and material skips in the related GitHub issue. Close an issue only after its change and relevant acceptance checks are complete. Keep commits atomic.
- Never commit subscriptions, private endpoints, keys, generated profiles, or private logs. Pass real lab subscriptions through the supported environment variable; redact evidence.

## Stable versions and GitHub releases

- Only a human can approve a specific revision as stable. Successful tests, a merge, or an agent's judgment do not grant this approval. An explicit approval already provided in the conversation remains valid for the specified revision.
- Stable version tags are immutable. Never force-update or reuse one for a different commit; corrections get a new version.
- Use the `release-vpn` skill when preparing or publishing a release. If it is unavailable, follow these rules and report that limitation.
- Run routine checks locally. Do not add GitHub Actions triggers for pushes, pull requests, tags, or schedules. Release automation is manual (`workflow_dispatch`) and targets an explicitly selected stable tag.
- Creating/pushing a stable tag does not authorize paid CI. Start a GitHub release workflow only when the user has explicitly authorized that run. Do not automatically retry failed runs.
- Before publishing, verify the selected tag resolves to the human-approved, locally tested commit. Release assets must come from that revision and include SHA-256 checksums.
- This application includes Go code, a Bun/OpenTUI interface, container configuration, and installer assets. Do not present a Go binary alone as the complete application. Describe the contents and remaining installation dependencies of the release archive accurately.
- Do not deploy or reconnect the live VPN as part of tagging or publishing a release.
