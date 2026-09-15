#!/usr/bin/env bash
# Compatibility launcher for the same interface as ./run.sh.
set -Eeuo pipefail
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd -P)
exec "$root/run.sh" "$@"
