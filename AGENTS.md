# AGENTS.md

This file provides guidance to coding agents when working with code in this repository.

## Project Overview

Go SDK for connecting to an avatar service via WebSocket. Sends PCM audio input and receives animation frame data through protocol buffer messaging.

## Build & Test Commands

```bash
# Run all tests
go test ./...

# Run tests with coverage (used in CI)
go test ./... -covermode=atomic -coverprofile=coverage.out

# Regenerate protobuf code (requires buf CLI)
cd proto && buf generate
```

## Architecture

### Session Lifecycle

1. **NewAvatarSession(opts...)** - Create session with functional options
2. **Init(ctx)** - Resolves `region="auto"` (the default) via the global bootstrap API and composes endpoint URLs (skipped when a concrete region or explicit endpoint URLs are configured), then HTTP POST to console endpoint to get session token
3. **Start(ctx)** - Dial WebSocket, perform v2 handshake (ClientConfigureSession → ServerConfirmSession)
4. **SendAudio(audio, end)** - Send PCM audio chunks; `end=true` marks stream end
5. **Close()** - Graceful WebSocket close, triggers OnClose callback. Call **ShutdownTelemetry()** when a short-lived process exits immediately after a session so pending metrics/traces are flushed.

### Key Files

- `avatar_session.go` - Core session implementation (Init, Start, SendAudio, Close, readLoop)
- `session_config.go` - SessionConfig struct and functional options (WithAPIKey, WithAvatarID, etc.)
- `bootstrap.go` - Global bootstrap API client. `resolveRegion` resolves `region="auto"` into a concrete ingress region via `POST https://global.spatialwalk.top/bootstrap` (request: app_id, sdk_version, region, platform; 5s timeout). Falls back to a process-level cached region or `DefaultRegion` ("us-west") on failure and never fails.
- `telemetry.go` - Process-wide unauthenticated OpenTelemetry metrics and traces. Uses `https://t.spatialwalk.top` by default, derives `/v1/metrics` and `/v1/traces`, and supports `ConfigureTelemetry("")` to disable export. No OpenTelemetry logs are emitted.
- `errors.go` - AvatarSDKError type with stable error codes
- `proto/message.proto` - Protocol buffer message definitions
- `proto/generated/` - Auto-generated protobuf Go code

### Message Types (proto/message.proto)

- `MESSAGE_CLIENT_CONFIGURE_SESSION` - Handshake: client sends audio config (plus optional `extra_params` map)
- `MESSAGE_SERVER_CONFIRM_SESSION` - Handshake: server returns connection_id
- `MESSAGE_CLIENT_AUDIO_INPUT` - Client sends audio chunks with req_id; the first chunk per req_id carries W3C `trace_context` when tracing is enabled
- `MESSAGE_SERVER_RESPONSE_ANIMATION` - Server sends animation frames
- `MESSAGE_SERVER_ERROR` - Server error with connection_id, req_id, code, message

### Telemetry

Metrics and traces are enabled by default and sent without authentication to `https://t.spatialwalk.top/v1/metrics` and `https://t.spatialwalk.top/v1/traces`. Configure the process-wide OTLP base endpoint before using a session:

```go
spatius.ConfigureTelemetry("https://telemetry.example.com")
// spatius.ConfigureTelemetry("")  // disable metrics and traces

// At process shutdown, if pending data must be flushed:
spatius.ShutdownTelemetry()
```

The first audio message for each request carries W3C `traceparent` context through the shared protobuf `TraceContext` field. Later chunks omit it. The SDK exports metrics and traces only; it does not upload telemetry logs.

### Async Callbacks

The `readLoop()` goroutine processes incoming messages and invokes:
- `TransportFrames(payload []byte, isLast bool)` - Animation frame received
- `OnError(error)` - Server error or connection error
- `OnClose()` - Session closed

Callbacks execute in separate goroutines to avoid blocking the read loop.

### WebSocket Auth Modes

- **Mobile (default)**: `X-App-ID` and `X-Session-Key` headers
- **Web**: `appId` and `sessionKey` query params (enable with `WithUseQueryAuth(true)`)

### LiveKit Egress Mode

When configured with `WithLiveKitEgress(config)`, audio and animation data are streamed to a LiveKit room via the egress service instead of being returned through the WebSocket connection. The egress configuration is sent via the `ClientConfigureSession` proto message.

To use LiveKit egress mode:
1. Configure the session with `WithLiveKitEgress(&LiveKitEgressConfig{...})`
2. Provide LiveKit connection details: URL, API key, API secret, room name, and publisher ID
3. The server will create an egress connection and stream output to the LiveKit room
4. The `TransportFrames` callback will not be invoked since data goes to LiveKit

```go
session := NewAvatarSession(
    WithLiveKitEgress(&LiveKitEgressConfig{
        URL:         "wss://livekit.example.com",
        APIKey:      "your-api-key",
        APISecret:   "your-api-secret",
        RoomName:    "room-name",
        PublisherID: "publisher-id",
    }),
    // ... other options
)
```

## Environment Variables (for examples)

```bash
AVATAR_API_KEY              # Console API key
AVATAR_APP_ID               # Application identifier
AVATAR_REGION               # Optional Spatius region, defaults to "auto" (resolved via the bootstrap API; falls back to us-west)
AVATAR_CONSOLE_ENDPOINT     # Optional Console API URL override
AVATAR_INGRESS_ENDPOINT     # Optional WebSocket ingress URL override
AVATAR_SESSION_AVATAR_ID    # Avatar to use
AVATAR_AUDIO_FILE           # Optional audio fixture path for examples
```
