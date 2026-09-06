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

## Region Resolution

By default the session region is `"auto"`: `Init` resolves the recommended
ingress region via the global bootstrap API and composes the endpoint URLs from
the result. Resolution failures never block session initialization — the region
falls back to the last successfully resolved region or `"us-west"`.

Pass a concrete region to skip resolution, or explicit endpoint URLs to override
region composition entirely:

```go
spatius.NewAvatarSession(
	// ...
	spatius.WithRegion("us-west"), // concrete region: no bootstrap call
)

spatius.NewAvatarSession(
	// ...
	spatius.WithConsoleEndpointURL("https://console.example.com/v1/console"),
	spatius.WithIngressEndpointURL("wss://api.example.com/v2/driveningress"),
)
```

## Session Extra Params

Optional extension parameters can be sent during the WebSocket session
handshake. Keys and values must be strings:

```go
spatius.NewAvatarSession(
	// ...
	spatius.WithExtraParams(map[string]string{
		"server_post_process": "false",
	}),
)
```

## Documentation

Detailed usage lives in [Spatius docs](https://docs.spatius.ai/sdk-reference/go-sdk/go-sdk).

## License

MIT
