#!/usr/bin/env bash
set -euo pipefail

repo_root="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
errors=0

fail() {
  echo "agent-docs: $*" >&2
  errors=$((errors + 1))
}

require_file() {
  if [[ ! -e "$repo_root/$1" ]]; then
    fail "missing $1"
  fi
}

require_text() {
  local file="$1"
  local text="$2"
  if ! grep -Fq "$text" "$repo_root/$file"; then
    fail "$file is missing canonical text: $text"
  fi
}

for file in AGENTS.md agent-manifest.json llms.txt llms-full.txt \
  docs/api/openapi.yaml docs/config/config.schema.json; do
  require_file "$file"
done

if [[ -f "$repo_root/agent-manifest.json" ]]; then
  if ! jq -e . "$repo_root/agent-manifest.json" >/dev/null 2>&1; then
    fail "agent-manifest.json is not valid JSON"
  else
    jq -e '
      .schema_version == "1.0" and
      .project.name == "LiveForge" and
      .project.repository == "https://github.com/im-pingo/liveforge" and
      (.capabilities | type == "array" and length > 0) and
      (.install | type == "array" and length > 0) and
      (.security | type == "object")
    ' "$repo_root/agent-manifest.json" >/dev/null || fail "agent-manifest.json is missing required fields"

    go_version="$(awk '$1 == "go" { print $2; exit }' "$repo_root/go.mod")"
    manifest_go="$(jq -r '.runtime.go // empty' "$repo_root/agent-manifest.json")"
    if [[ -z "$go_version" || "$manifest_go" != ">=$go_version" ]]; then
      fail "manifest runtime.go must match go.mod (expected >=$go_version)"
    fi

    for protocol in rtmp rtsp srt webrtc hls ll-hls dash http-flv fmp4 gb28181 websocket; do
      jq -e --arg id "$protocol" '.capabilities[] | select(.id == $id)' \
        "$repo_root/agent-manifest.json" >/dev/null || fail "manifest is missing capability $protocol"
    done

    jq -e '
      .operations.console.views == ["Streams", "GB28181", "Config", "Cluster", "SIP Calls", "Storage", "Security"] and
      .operations.console.recent_audit == "inside Security; not a separate tab"
    ' "$repo_root/agent-manifest.json" >/dev/null || fail "manifest console tabs do not match the canonical seven-tab UI"

    while IFS= read -r doc; do
      [[ -z "$doc" ]] && continue
      require_file "$doc"
    done < <(jq -r '
      .source_of_truth |
      to_entries[] |
      select(.value | type == "string" and (endswith(".md") or endswith(".json") or endswith(".yaml") or endswith("/"))) |
      .value
    ' "$repo_root/agent-manifest.json")
  fi
fi

canonical_tabs='Streams, GB28181, Config, Cluster, SIP Calls, Storage, and Security'
for file in README.md llms.txt llms-full.txt docs/PROGRESS.md; do
  require_text "$file" "$canonical_tabs"
  require_text "$file" 'Recent Audit is a surface inside Security, not a separate tab.'
done
require_text README.zh-CN.md "$canonical_tabs"
require_text README.zh-CN.md 'Recent Audit 是 Security 内部的界面，不是单独的第八个标签页。'

if [[ -f "$repo_root/llms.txt" ]]; then
  grep -Fq 'agent-manifest.json' "$repo_root/llms.txt" || fail "llms.txt must link agent-manifest.json"
  grep -Fq 'llms-full.txt' "$repo_root/llms.txt" || fail "llms.txt must link llms-full.txt"
fi

if [[ -f "$repo_root/AGENTS.md" ]]; then
  grep -Fq 'agent-manifest.json' "$repo_root/AGENTS.md" || fail "AGENTS.md must name agent-manifest.json"
  grep -Fq 'llms.txt' "$repo_root/AGENTS.md" || fail "AGENTS.md must name llms.txt"
  grep -Fq 'go test ./...' "$repo_root/AGENTS.md" || fail "AGENTS.md must define the baseline test command"
fi

if [[ -f "$repo_root/docs/api/openapi.yaml" ]]; then
  grep -Fq 'openapi: 3.1.0' "$repo_root/docs/api/openapi.yaml" || fail "OpenAPI document must declare version 3.1.0"
  for path in '/api/v1/streams' '/api/v1/server/health' '/api/v1/server/info' '/api/v1/server/stats' '/api/v1/gb28181/devices' '/api/v1/gb28181/channels' '/api/v1/gb28181/sessions'; do
    grep -Fq "  $path:" "$repo_root/docs/api/openapi.yaml" || fail "OpenAPI document is missing $path"
  done
fi

if [[ -f "$repo_root/docs/config/config.schema.json" ]] && ! jq -e '
  .type == "object" and
  (.properties | has("server")) and
  (.properties | has("audio_codec")) and
  (.properties | has("api"))
' "$repo_root/docs/config/config.schema.json" >/dev/null 2>&1; then
  fail "config schema is missing required top-level sections"
fi

if [[ "${CHECK_AGENT_DOCS_DIFF:-0}" == "1" ]]; then
  base_ref="${BASE_REF:-HEAD^}"
  if ! git -C "$repo_root" rev-parse --verify "$base_ref" >/dev/null 2>&1; then
    fail "cannot resolve BASE_REF=$base_ref for documentation diff check"
  else
    changed_files="$({
      git -C "$repo_root" diff --name-only "$base_ref...HEAD"
      git -C "$repo_root" diff --name-only
      git -C "$repo_root" ls-files --others --exclude-standard
    } | sort -u)"
    source_changed=0
    agent_docs_changed=0
    while IFS= read -r file; do
      [[ -z "$file" ]] && continue
      case "$file" in
        cmd/*|config/*|core/*|module/*|pkg/*|test/*|tools/*|configs/*|Dockerfile|Makefile|docker-compose.yaml|go.mod|go.sum|.github/workflows/*)
          source_changed=1
          ;;
      esac
      case "$file" in
        AGENTS.md|agent-manifest.json|llms.txt|llms-full.txt|README.md|README.zh-CN.md|docs/*)
          agent_docs_changed=1
          ;;
      esac
    done <<< "$changed_files"
    if (( source_changed == 1 && agent_docs_changed == 0 )); then
      fail "source changes must update at least one AI-facing document"
    fi
  fi
fi

if (( errors > 0 )); then
  echo "agent-docs: $errors error(s)" >&2
  exit 1
fi

echo "agent-docs: all checks passed"
