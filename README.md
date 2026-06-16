# Spatius Golang SDK

[![codecov](https://codecov.io/github/spatius-ai/spatius-sdk-go/graph/badge.svg?token=U8TXD927WQ)](https://codecov.io/github/spatius-ai/spatius-sdk-go)
[![Go Report Card](https://goreportcard.com/badge/github.com/spatius-ai/spatius-sdk-go)](https://goreportcard.com/report/github.com/spatius-ai/spatius-sdk-go)
[![Go Reference](https://pkg.go.dev/badge/github.com/spatius-ai/spatius-sdk-go.svg)](https://pkg.go.dev/github.com/spatius-ai/spatius-sdk-go)

Go SDK for Spatius avatar sessions.

## Install

```bash
go get github.com/spatius-ai/spatius-sdk-go
```

## Quick Start

```go
package main

import (
	"context"
	"log"
	"time"

	spatius "github.com/spatius-ai/spatius-sdk-go"
)

func main() {
	ctx := context.Background()

	session := spatius.NewAvatarSession(
		spatius.WithAPIKey("your-api-key"),
		spatius.WithAppID("your-app-id"),
		spatius.WithAvatarID("your-avatar-id"),
		spatius.WithExpireAt(time.Now().Add(5*time.Minute).UTC()),
		spatius.WithTransportFrames(func(data []byte, last bool) {
			// Handle animation frame bytes.
		}),
	)

	if err := session.Init(ctx); err != nil {
		log.Fatal(err)
	}
	defer session.Close()

	if _, err := session.Start(ctx); err != nil {
		log.Fatal(err)
	}

	audioBytes := []byte{} // Replace with mono PCM audio bytes.
	reqID, err := session.SendAudio(audioBytes, true)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("sent request %s", reqID)
}
```

## Ogg Opus Audio

The SDK supports `ogg_opus` sessions in two modes. In both modes, configure the
session with `WithAudioFormat(spatius.AudioFormatOggOpus)` so the WebSocket
handshake negotiates `AUDIO_FORMAT_OGG_OPUS`.

### Pre-encoded Ogg Opus

If your application already has a continuous mono Ogg Opus stream for a request,
send those bytes directly. The SDK forwards them unchanged.

```go
session := spatius.NewAvatarSession(
	spatius.WithAPIKey("your-api-key"),
	spatius.WithAppID("your-app-id"),
	spatius.WithAvatarID("your-avatar-id"),
	spatius.WithExpireAt(time.Now().Add(5*time.Minute).UTC()),
	spatius.WithAudioFormat(spatius.AudioFormatOggOpus),
)

reqID, err := session.SendAudio(oggOpusBytes, true)
```

### Encode PCM to Ogg Opus

If your application has raw PCM, enable the internal encoder. `SendAudio` then
accepts mono 16-bit little-endian PCM at the configured sample rate and sends a
continuous Ogg Opus stream for each request ID.

```go
session := spatius.NewAvatarSession(
	spatius.WithAPIKey("your-api-key"),
	spatius.WithAppID("your-app-id"),
	spatius.WithAvatarID("your-avatar-id"),
	spatius.WithExpireAt(time.Now().Add(5*time.Minute).UTC()),
	spatius.WithAudioFormat(spatius.AudioFormatOggOpus),
	spatius.WithSampleRate(24000),
	spatius.WithBitrate(32000),
	spatius.WithOggOpusEncoder(&spatius.OggOpusEncoderConfig{
		FrameDurationMS: 20,
		Application:     spatius.OggOpusApplicationAudio,
	}),
	spatius.WithOnEncodedAudio(func(reqID string, payload []byte) {
		// Optional: persist or inspect the completed Ogg Opus stream.
	}),
)

reqID, err := session.SendAudio(pcmS16LEBytes, true)
```

Supported encoder sample rates are `8000`, `12000`, `16000`, `24000`, and
`48000` Hz. Supported frame durations are `10`, `20`, `40`, and `60` ms.
Supported applications are `audio`, `voip`, and `restricted_lowdelay`.

The internal encoder uses `github.com/hraban/opus`, which typically requires cgo
and a system `libopus` installation at build and runtime.

## Documentation

Detailed usage lives in [Spatius docs](https://docs.spatius.ai/sdk-reference/go-sdk/go-sdk).

## License

MIT
