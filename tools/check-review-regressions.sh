#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
soak_seconds="${LIVEFORGE_REVIEW_SOAK_SECONDS:-60}"
case "$soak_seconds" in
  ''|*[!0-9]*) echo "LIVEFORGE_REVIEW_SOAK_SECONDS must be an integer from 1 to 300" >&2; exit 2 ;;
esac
if (( soak_seconds < 1 || soak_seconds > 300 )); then
  echo "LIVEFORGE_REVIEW_SOAK_SECONDS must be from 1 to 300" >&2
  exit 2
fi

tools/check-agent-docs_test.sh
go test -race ./core ./module/cluster -run 'TestStreamMultiStreamConcurrentIngress|TestStreamMultiReaderIngress|TestStreamPublisherReplacementDuringIngress|TestEventBusLifecycleQueueBackpressureIsBoundedAndObservable|TestCluster' -count=10 -timeout=5m
go test -race ./config ./config/runtime ./core ./module/api ./module/webrtc ./module/record -run 'TestDocumentHistory|TestManager|TestConfig|TestRegistered|TestClusterRegisteredPermissions|TestConsoleLogout|TestSignalingBodyTimeout|TestManagementBodyTimeout|TestRecordingListIndex|TestLocalStorageList|TestStreamPagination|TestListPagination|TestAudit|Test.*Simulcast|TestWHIPThreeRID' -count=1 -timeout=5m
go test ./pkg/codec/h264 -run '^$' -fuzz '^FuzzParseSPS$' -fuzztime=5s -parallel=2
go test ./pkg/muxer/fmp4 -run '^$' -fuzz '^FuzzParseAVCCDimensions$' -fuzztime=5s -parallel=2
go test ./pkg/rtp -run '^$' -fuzz '^FuzzH265FUReassembly$' -fuzztime=5s -parallel=2
go test ./pkg/rtp -run '^$' -fuzz '^FuzzVP8Descriptor$' -fuzztime=5s -parallel=2
LIVEFORGE_REQUIRE_BROWSER=1 go test ./module/api -run '^TestConsole' -count=1 -timeout=5m
LIVEFORGE_REQUIRE_BROWSER=1 LIVEFORGE_PROTOCOL_MATRIX_SOAK="${soak_seconds}s" CGO_ENABLED=1 \
  go test -tags audiocodec -race ./test/integration -run '^TestSIPGB28181WHIPBrowserBridgeMatrix$' -count=1 -timeout=20m
