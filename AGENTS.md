# AGENTS.md

Guidance for coding agents working in this repository.

## What this repo is

Go SDK for WebSocket-based avatar sessions, published as `github.com/spatius-ai/spatius-sdk-go`. Clients stream audio to the backend and receive animation frames back. The wire protocol is protobuf (`proto/message.proto`).

## Layout

- `avatar_session.go` — `AvatarSession`: session-token HTTP exchange, WebSocket handshake (v2 protocol), audio send, frame receive loop. Session flow: `NewAvatarSession(...)` → `Init(ctx)` → `Start(ctx)` → `SendAudio(...)` → `Close()`.
- `session_config.go` — `SessionConfig`, LiveKit/Agora egress configs, functional options (`WithAPIKey`, `WithAvatarID`, etc.).
- `bootstrap.go` — resolves `region="auto"` via the global bootstrap API; process-wide 5-minute cache, never fails.
- `net.go` — process-wide TLS config and HTTP transport shared by HTTP and WebSocket connections.
- `prewarm.go`, `token_cache.go` — optional warm-up (region resolution, TLS, session token) ahead of session dispatch.
- `telemetry.go` — process-wide OpenTelemetry metrics/traces, on by default; `ConfigureTelemetry("")` disables.
- `errors.go` — `AvatarSDKError` with stable error codes.
- `proto/message.proto`, `proto/generated/` — protocol definition and generated code (regenerate with `cd proto && buf generate`).

Behavioral facts that are easy to get wrong:

- Auth is header-based by default; `WithUseQueryAuth(true)` switches to query-param auth.
- Egress modes (LiveKit/Agora) stream output to a room/channel instead of the WebSocket; the `TransportFrames` callback is not invoked in egress mode, and `Interrupt` only works there.
- The first audio message per request carries W3C trace context; later chunks omit it.

## Build and development commands

```bash
go test ./...                                                # run tests
go test ./... -covermode=atomic -coverprofile=coverage.out   # run tests with coverage (used in CI)
cd proto && buf generate                                     # regenerate protobuf code after editing message.proto
```

## Rules for agents

- Never add or change content in `README.md` unless explicitly told to.
- After editing `proto/message.proto`, always regenerate the code under `proto/generated/` with `buf generate`.
