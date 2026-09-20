---
name: release-vpn
description: Prepare a human-approved stable version or publish a manual GitHub release of blockedby/local-vpn-kde, using local validation and versioned installer assets.
---

# Release local-vpn-kde

Work in the checkout whose Git origin is `blockedby/local-vpn-kde`; the usual local path is `~/code/tools/local-vpn-kde`. Read its `AGENTS.md`. Do not use the former `vibe-practicum-vpn` repository.

## Authorization and scope

Distinguish three actions: preparing a candidate, marking an approved commit stable, and running GitHub release automation. Honor existing explicit user authorization for each; invoking this skill alone does not authorize all three.

- A human decides which exact revision is stable. Tests and agent judgment cannot make that decision. If approval said “current version,” resolve it to the commit current when approval was given, not subsequent changes.
- Never move, overwrite, or reuse a published stable tag. Use a new semantic version for a new revision.
- Routine checks are local. GitHub Actions must have only a manual `workflow_dispatch` trigger: no push, PR, tag, or schedule triggers. Pushing a tag must not start CI.
- Start a release workflow only with explicit authorization for that run. Inspect a failure before any retry; another paid run needs authorization unless retries were explicitly included.
- Release preparation never implies permission to restart, reconnect, or alter the host VPN.

## Prepare and validate locally

1. Inspect Git status, origin, current revision, tags, and existing release tooling. Preserve unrelated edits. Select the intended immutable commit and version; check for local and remote tag collisions.
2. Use a clean isolated checkout of that revision if the current tree differs. Run `go test -race ./...`. In `scripts/vpnkit/tui`, install locked dependencies if needed, then run `bun run check` and `bun test`.
3. Run relevant existing functional tests in `test/`. For installation/routing/runtime acceptance, use disposable Docker/Podman labs, especially `test/vpnkit-local-install-container-test.sh`; inspect its supported inputs first. Keep distinct containers, networks, volumes, and state. Never substitute live host mutations for missing lab prerequisites.
4. Provide private subscription input through the lab's environment mechanism, never through tracked files or logged command arguments. Report missing prerequisites and skipped coverage honestly.
5. Record commit, validation evidence, limitations, and release contents in the associated issue or release preparation report. Existing fresh evidence for the exact revision may be reused; do not claim it validates later changes.

## Mark stable and publish

When the human-approved revision is known and the requested checks pass, create an annotated version tag on that exact commit. Push it only within the user's authorized scope. If only a stable checkpoint was requested, stop after reporting the tag and commit; do not run Actions or publish a release.

For an authorized GitHub release run:

1. Inspect `.github/workflows/release.yml` (or the repository's actual release workflow) and packaging helper. Verify they exist, accept an explicit tag, checkout that tag, have no automatic triggers, and package that revision. Do not invent a workflow invocation when the file is absent.
2. Verify the remote tag resolves to the approved, locally tested commit. Check whether the release already exists; do not overwrite its assets automatically.
3. Run the workflow with the selected tag using its actual input names. Follow that one run and inspect its outcome. A failed build is not a release.
4. Verify the GitHub release references the intended tag, expected assets exist, and archive hashes match the published SHA-256 checksums. Report the release URL, tag/commit, asset contents, and meaningful limitations.

The distributable is an installer archive containing the required application assets, potentially including prebuilt Go executables. Bun/OpenTUI and the Docker gateway remain part of the application. Do not call an isolated Go executable a standalone VPN application; identify installation dependencies and platform/architecture support accurately. Exclude secrets, runtime state, private logs, caches, and unrelated development artifacts from archives.
