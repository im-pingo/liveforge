# Task 2 SIP Lab Fix Report

Date: 2026-08-26
Base reviewed commit: `ed0b6eb`
Commit: created with the requested author and message after verification; the
final hash is reported by the handoff and `git log -1`.

## Scope

The reviewed Task 2 SIP protocol lab implementation was hardened on the current
branch. Existing unrelated work in `docs/architecture.zh-CN.md` was preserved
and is intentionally not part of this fix.

## Changes

- `module/sip/dispatch.go`: registered a transport-level ACK consumer. A
  dialog ACK for a successful INVITE is consumed without generating the
  default unhandled 405 response. Existing SIP service mocks and GB28181
  dispatch handlers remain unchanged.
- `module/sip/dispatch_test.go`: added regression coverage that verifies the
  initialized SIP service registers an ACK consumer.
- `module/sipgateway/lab.go`: derived protocol signaling context from both the
  caller context and the session stop context; coordinated stop-before-start
  races; removed the lifecycle mutex from network signaling; synchronized
  resource assignment and cleanup; and made `Gateway.Close` cancel blocked
  starts and release RTP/RTCP resources promptly. BYE transaction handling now
  consumes provisional responses until a final response with status >= 200.
- `module/sipgateway/lab_test.go`: added regressions for dialog ACK handling,
  blocked-start shutdown and socket reuse, BYE `100 Trying` followed by
  `200 OK`, and transportless manager availability semantics.
- `module/sipgateway/status.go`: added the explicit `contract` session state;
  standalone `NewLabManager` sessions are contract-only and never active.
- `module/gb28181/session.go` and `module/gb28181/lab_test.go`: retained
  persistent GB28181 lab availability as unimplemented while preserving invalid
  request validation and making the Task 1 contract tests assert unavailable
  behavior rather than a started transportless session.
- `agent-manifest.json`, `llms-full.txt`, `llms.txt`, `README.md`,
  `README.zh-CN.md`, and `docs/recipes/protocol-test-lab.md`: documented the
  supported SIP provider methods, PCMA/PCMU publish and receive workflow,
  cancellable stop/close behavior, contract-only standalone manager semantics,
  and explicit persistent GB28181 unavailability.

## TDD Evidence

Each reviewed SIP defect was tested red before the implementation fix and green
afterward:

| Defect | Red command/result | Green command/result |
| --- | --- | --- |
| INVITE dialog ACK | `go test -count=1 ./module/sipgateway -run '^TestSIPLabPublishConsumesDialogACKWithout405$' -v` failed with `SIP request handler not found` and `405 Method Not Allowed`. | The focused lab test passed after the transport consumer was added; direct transport coverage also passes with `go test -count=1 ./module/sip -run '^TestSIPServiceRegistersDialogACKConsumer$' -v`. |
| Close of blocked start | `go test -count=1 ./module/sipgateway -run '^TestSIPLabGatewayCloseCancelsBlockedStart$' -v` blocked in `Gateway.Close`. | The same test passed after stop context coordination and cleanup synchronization were added; it verifies prompt close, terminal stop, and RTP/RTCP port reuse. |
| BYE provisional response | `go test -count=1 ./module/sipgateway -run '^TestSIPLabBYEConsumesProvisionalResponseBeforeFinal$' -v` failed with `lab SIP BYE rejected: 100 Trying`. | The same command passed after response collection was changed to wait for status >= 200. |
| Standalone manager availability | `go test -count=1 ./module/sipgateway -run '^TestLabManagerMarksTransportlessSessionAsContractOnly$' -v` failed with `transportless session state = active, want contract`. | The same command passed after adding `LabSessionStateContract` and contract-only behavior. |

The existing GB28181 Task 1 contract tests initially exposed the pre-existing
unimplemented-manager expectation mismatch during `go test ./...`; they now
pass with explicit invalid-request and unavailable-manager assertions.

## Verification

All commands below were run against the final working tree:

- `go test -count=1 ./...` -> passed; every package reported `ok`.
- `go test -count=1 ./module/sipgateway -run 'Lab|SelfTest' -v` -> passed,
  including publish, receive, blocked-close, BYE provisional-response, and
  contract tests. Focused output contained no unhandled ACK 405 warning.
- `go test -count=1 -race ./module/sipgateway` -> `ok`.
- `go test -count=1 -race ./module/sip` -> `ok`.
- `go test -count=1 ./module/sip -run 'Listener|Transport|Dispatch' -v` ->
  all dispatch and listener/transport tests passed.
- `go test -count=1 ./module/api` -> `ok`.
- `go vet ./module/sip ./module/sipgateway ./module/gb28181 ./module/api` ->
  passed with no output.
- `tools/check-agent-docs_test.sh` -> `agent-docs: all checks passed` and
  `agent documentation check passed`.
- `CHECK_AGENT_DOCS_DIFF=1 BASE_REF=ed0b6eb tools/check-agent-docs.sh` ->
  `agent-docs: all checks passed`.
- `jq empty agent-manifest.json` -> passed.
- `git diff --check` -> passed.
- `gofmt` was run on all changed Go files.

## Remaining Concerns

The focused SIP lab tests still emit sipgo warnings such as `UDP ref went
negative on try close` during transport teardown. They are existing library
cleanup warnings, do not produce ACK 405 responses, and the regression test
proves that the RTP/RTCP ports are released and reusable. No test, race, vet,
documentation, or diff-check failures remain.
