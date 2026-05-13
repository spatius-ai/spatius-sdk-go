# spatius-sdk-go

[![codecov](https://codecov.io/gh/spatius-ai/spatius-sdk-go/graph/badge.svg?token=Y5Q4M43OTM)](https://codecov.io/gh/spatius-ai/spatius-sdk-go)
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

Detailed usage lives in [Spatius docs](https://docs.spatius.ai/sdk-reference/go-sdk/go-sdk).
