#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if ! "$repo_root/tools/check-agent-docs.sh"; then
  echo "agent documentation check failed" >&2
  exit 1
fi

echo "agent documentation check passed"
