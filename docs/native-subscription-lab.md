# Opt-in native VPN subscription lab

This test uses your real subscription and contacts external servers. It does
not install or reconnect the host VPN. The native gateway and OpenVPN client
run inside a disposable Docker container; Docker is test isolation only.

## Private input

In the native-runtime worktree root, create `.env` with your editor:

```dotenv
VPNKIT_LAB_SUBSCRIPTION_URL="https://example.invalid/replace-with-your-private-subscription"
```

Replace the placeholder, then run `chmod 600 .env`. `.env` and `.env.local` are
Git-ignored in this worktree. Do not paste the subscription into a command line,
commit it, or send it in chat. For an alternate file, ensure it is private and
outside tracked source files.

The reader treats values literally: no shell sourcing, substitutions, variable
expansion, or execution. It requires exactly one HTTPS subscription URL, an
owned regular file without symlinks/hardlinks, and private permissions. Other
dotenv keys are ignored and are not passed into the container.

## Run

From the worktree root:

```sh
bash test/native-gateway-subscription-container-test.sh --env-file .env --max-candidates 50
```

The test requires Docker, Go, Python 3, timeout, and the locally prepared pinned
lab image used by the synthetic native VPN tests. It does not automatically
pull an image. A missing or invalid private input must fail before live testing.

Only a temporary subscription file is mounted read-only, not the full `.env`,
repository, host runtime socket, or host namespaces. The URL must not appear in
Docker arguments, container environment, or reported logs. Temporary files and
the test container are removed on exit.

The real test is separate from routine offline/synthetic regressions. Run it
explicitly; do not add it to CI or normal test commands.

## Recorded real-subscription result

The real container run passed on the uncommitted native-runtime worktree:

- Production non-applying `vibe-vpn test`: 20 tested, 8 successful.
- Exact selected outbound passed the Docker-network SOCKS control.
- Restricted native service and localhost OpenVPN client transferred 64 KiB
  from the same HTTPS benchmark target, with HTTP 2xx and tunnel byte movement.
- After sing-box loss, HTTPS failed while the client and both `/1` routes remained.

The earlier test differed from production selection: it tried only the first
three supported entries, which also failed the no-TUN control. It also omitted
endpoint pre-resolution. A separate fixture shutdown bug prevented retries;
three sequential synthetic service lifecycles now cover that regression.

`--max-candidates` accepts 1–50 (default 50). The test uses a 64 KiB benchmark
limit per candidate with duration mode disabled and a bounded overall run.
No endpoint, subscription value, response body, or private configuration belongs
in test reports. This result is not a release or installation approval.

## Interpretation

A successful real test can demonstrate subscription loading and native VPN
traffic within the container. The container's outer `eth0` connection may itself
traverse an already-active host VPN; this does not prove direct ISP egress.
Do not infer real KDE/systemd/autostart, tunneled DNS, or a host kill switch after
OpenVPN restores routes from this result. Record actual stage results and any
skips; missing private input is not a pass.
