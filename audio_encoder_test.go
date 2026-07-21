package spatiussdkgo

import (
	"bytes"
	"encoding/binary"
	"math"
	"runtime"
	"strings"
	"testing"

	"github.com/hraban/opus"
)

func generateSpeechLikePCM(sampleRate int, durationSeconds int) []byte {
	sampleCount := sampleRate * durationSeconds
	pcm := make([]byte, sampleCount*opusPCMBytesPerSample)

	for i := 0; i < sampleCount; i++ {
		timeSeconds := float64(i) / float64(sampleRate)
		fade := min(
			1.0,
			float64(i)/(float64(sampleRate)*0.05),
			float64(sampleCount-i)/(float64(sampleRate)*0.05),
		)
		envelope := fade * (0.55 + 0.45*(0.5+0.5*math.Sin(2*math.Pi*3*timeSeconds)))
		value := envelope * (0.50*math.Sin(2*math.Pi*180*timeSeconds) +
			0.22*math.Sin(2*math.Pi*360*timeSeconds+0.3) +
			0.08*math.Sin(2*math.Pi*720*timeSeconds+1.0))
		binary.LittleEndian.PutUint16(pcm[i*opusPCMBytesPerSample:], uint16(int16(value*32767)))
	}

	return pcm
}

func parseOggPackets(t *testing.T, stream []byte) ([][]byte, uint64) {
	t.Helper()

	var packets [][]byte
	var pendingPacket []byte
	var finalGranule uint64
	for offset := 0; offset < len(stream); {
		if offset+27 > len(stream) || !bytes.Equal(stream[offset:offset+4], []byte("OggS")) {
			t.Fatalf("invalid Ogg page at offset %d", offset)
		}

		segmentCount := int(stream[offset+26])
		segmentTableStart := offset + 27
		segmentTableEnd := segmentTableStart + segmentCount
		if segmentTableEnd > len(stream) {
			t.Fatalf("truncated Ogg segment table at offset %d", offset)
		}

		finalGranule = binary.LittleEndian.Uint64(stream[offset+6 : offset+14])
		payloadOffset := segmentTableEnd
		for _, segmentSizeByte := range stream[segmentTableStart:segmentTableEnd] {
			segmentSize := int(segmentSizeByte)
			segmentEnd := payloadOffset + segmentSize
			if segmentEnd > len(stream) {
				t.Fatalf("truncated Ogg payload at offset %d", payloadOffset)
			}
			pendingPacket = append(pendingPacket, stream[payloadOffset:segmentEnd]...)
			payloadOffset = segmentEnd
			if segmentSize < 255 {
				packets = append(packets, append([]byte(nil), pendingPacket...))
				pendingPacket = nil
			}
		}

		offset = payloadOffset
	}

	if len(pendingPacket) > 0 {
		t.Fatal("unterminated Ogg packet")
	}
	return packets, finalGranule
}

func pcmCosine(left []byte, right []int16) float64 {
	if len(left) != len(right)*opusPCMBytesPerSample {
		return 0
	}

	var dotProduct float64
	var leftEnergy float64
	var rightEnergy float64
	for i, rightSample := range right {
		leftSample := int16(binary.LittleEndian.Uint16(left[i*opusPCMBytesPerSample:]))
		leftValue := float64(leftSample)
		rightValue := float64(rightSample)
		dotProduct += leftValue * rightValue
		leftEnergy += leftValue * leftValue
		rightEnergy += rightValue * rightValue
	}

	return dotProduct / math.Sqrt(leftEnergy*rightEnergy)
}

func TestOggOpusAudio64KQualityAndTimingMeetCodecGate(t *testing.T) {
	const (
		sampleRate      = 24000
		frameDurationMS = 20
		expectedBitrate = 64000
		durationSeconds = 3
	)

	pcm := generateSpeechLikePCM(sampleRate, durationSeconds)
	encoder, err := NewOggOpusStreamEncoder(sampleRate, expectedBitrate, &OggOpusEncoderConfig{
		FrameDurationMS: frameDurationMS,
		Application:     OggOpusApplicationAudio,
	}, true)
	if err != nil {
		t.Fatalf("NewOggOpusStreamEncoder returned error: %v", err)
	}

	bitrate, err := encoder.encoder.Bitrate()
	if err != nil || bitrate != expectedBitrate {
		t.Fatalf("expected bitrate %d, got %d (error: %v)", expectedBitrate, bitrate, err)
	}
	vbr, err := encoder.encoder.VBR()
	if err != nil || !vbr {
		t.Fatalf("expected VBR to be enabled, got %t (error: %v)", vbr, err)
	}
	complexity, err := encoder.encoder.Complexity()
	if err != nil || complexity != 10 {
		t.Fatalf("expected complexity 10, got %d (error: %v)", complexity, err)
	}
	dtx, err := encoder.encoder.DTX()
	if err != nil || dtx {
		t.Fatalf("expected DTX to be disabled, got %t (error: %v)", dtx, err)
	}
	fec, err := encoder.encoder.InBandFEC()
	if err != nil || fec {
		t.Fatalf("expected in-band FEC to be disabled, got %t (error: %v)", fec, err)
	}

	var completedStream []byte
	chunkBytes := sampleRate * opusPCMBytesPerSample / 10
	for offset := 0; offset < len(pcm); offset += chunkBytes {
		end := offset+chunkBytes >= len(pcm)
		chunk, encodeErr := encoder.Encode(pcm[offset:min(offset+chunkBytes, len(pcm))], end)
		if encodeErr != nil {
			t.Fatalf("Encode returned error: %v", encodeErr)
		}
		if chunk.CompletedStream != nil {
			completedStream = chunk.CompletedStream
		}
	}
	if completedStream == nil {
		t.Fatal("expected a completed Ogg Opus stream")
	}

	packets, finalGranule := parseOggPackets(t, completedStream)
	if len(packets) < 3 {
		t.Fatalf("expected Opus headers and audio packets, got %d packets", len(packets))
	}
	if !bytes.HasPrefix(packets[0], []byte("OpusHead")) {
		t.Fatalf("expected first packet to be OpusHead, got %q", packets[0][:min(8, len(packets[0]))])
	}
	if !bytes.HasPrefix(packets[1], []byte("OpusTags")) {
		t.Fatalf("expected second packet to be OpusTags, got %q", packets[1][:min(8, len(packets[1]))])
	}
	headerCount := 0
	for _, packet := range packets {
		if bytes.HasPrefix(packet, []byte("OpusHead")) {
			headerCount++
		}
	}
	if headerCount != 1 {
		t.Fatalf("expected one continuous Ogg Opus stream, got %d OpusHead packets", headerCount)
	}

	preSkip48K := int(binary.LittleEndian.Uint16(packets[0][10:12]))
	sampleScale := 48000 / sampleRate
	expectedOutputSamples := len(pcm) / opusPCMBytesPerSample
	outputSamplesFromGranule := (int(finalGranule) - preSkip48K) / sampleScale
	difference := outputSamplesFromGranule - expectedOutputSamples
	if difference < 0 {
		difference = -difference
	}
	if difference > 1 {
		t.Fatalf("expected granule timing within one sample, got difference %d", difference)
	}

	decoder, err := opus.NewDecoder(sampleRate, opusEncoderChannels)
	if err != nil {
		t.Fatalf("create Opus decoder: %v", err)
	}
	frameSize := sampleRate * frameDurationMS / 1000
	decodedWithPreSkip := make([]int16, 0, len(packets[2:])*frameSize)
	for i, packet := range packets[2:] {
		frame := make([]int16, frameSize)
		n, decodeErr := decoder.Decode(packet, frame)
		if decodeErr != nil {
			t.Fatalf("decode audio packet %d: %v", i, decodeErr)
		}
		decodedWithPreSkip = append(decodedWithPreSkip, frame[:n]...)
	}

	preSkipSamples := preSkip48K / sampleScale
	if len(decodedWithPreSkip) < preSkipSamples+expectedOutputSamples {
		t.Fatalf("decoded output is too short: got %d samples, need %d", len(decodedWithPreSkip), preSkipSamples+expectedOutputSamples)
	}
	decoded := decodedWithPreSkip[preSkipSamples : preSkipSamples+expectedOutputSamples]
	if cosine := pcmCosine(pcm, decoded); cosine < 0.99 {
		t.Fatalf("expected PCM cosine >= 0.99, got %.6f", cosine)
	}
}

func TestOggOpusLongRunningStreamDoesNotRetainMemory(t *testing.T) {
	const (
		frameDurationMS = 20
		framesPerMinute = 60_000 / frameDurationMS
	)

	encoder, err := NewOggOpusStreamEncoder(24000, 32000, &OggOpusEncoderConfig{
		FrameDurationMS: frameDurationMS,
		Application:     OggOpusApplicationVoIP,
	}, false)
	if err != nil {
		t.Fatalf("NewOggOpusStreamEncoder returned error: %v", err)
	}
	pcmFrame := make([]byte, 480*opusPCMBytesPerSample)

	encodeFrames := func(count int) {
		t.Helper()
		for range count {
			if _, encodeErr := encoder.Encode(pcmFrame, false); encodeErr != nil {
				t.Fatalf("Encode returned error: %v", encodeErr)
			}
		}
	}
	retainedHeap := func() uint64 {
		t.Helper()
		runtime.GC()
		var stats runtime.MemStats
		runtime.ReadMemStats(&stats)
		return stats.HeapAlloc
	}

	encodeFrames(100)
	encodeFrames(framesPerMinute)
	retainedAfterOneMinute := retainedHeap()
	encodeFrames(framesPerMinute)
	retainedAfterTwoMinutes := retainedHeap()
	runtime.KeepAlive(encoder)

	retainedGrowth := int64(retainedAfterTwoMinutes) - int64(retainedAfterOneMinute)
	if retainedGrowth >= 128*1024 {
		t.Fatalf("retained memory grew by %d bytes during streaming", retainedGrowth)
	}

	if _, err := encoder.Encode(nil, true); err != nil {
		t.Fatalf("finalize stream: %v", err)
	}
}

func TestBuildOggLacingValuesTerminatesPacketsDivisibleBy255Once(t *testing.T) {
	t.Parallel()

	if got := buildOggLacingValues(bytes.Repeat([]byte{'x'}, 255)); !bytes.Equal(got, []byte{255, 0}) {
		t.Fatalf("expected lacing values [255 0], got %v", got)
	}
}

func TestNewOggOpusStreamEncoderRejectsInvalidConfig(t *testing.T) {
	t.Parallel()

	_, err := NewOggOpusStreamEncoder(44100, 0, nil, false)
	if err == nil || !strings.Contains(err.Error(), "supports sample rates") {
		t.Fatalf("expected unsupported sample rate error, got %v", err)
	}

	_, err = NewOggOpusStreamEncoder(24000, 0, &OggOpusEncoderConfig{
		FrameDurationMS: 15,
		Application:     OggOpusApplicationAudio,
	}, false)
	if err == nil || !strings.Contains(err.Error(), "supports frame durations") {
		t.Fatalf("expected unsupported frame duration error, got %v", err)
	}

	_, err = NewOggOpusStreamEncoder(24000, 0, &OggOpusEncoderConfig{
		FrameDurationMS: 20,
		Application:     OggOpusApplication("speech"),
	}, false)
	if err == nil || !strings.Contains(err.Error(), "application must be one of") {
		t.Fatalf("expected unsupported application error, got %v", err)
	}
}

func TestResolveOggOpusEncoderConfigDefaultsToVoIP(t *testing.T) {
	t.Parallel()

	resolved := resolveOggOpusEncoderConfig(nil)
	if resolved.FrameDurationMS != 20 {
		t.Fatalf("expected FrameDurationMS to be 20, got %d", resolved.FrameDurationMS)
	}
	if resolved.Application != OggOpusApplicationVoIP {
		t.Fatalf("expected Application to be %q, got %q", OggOpusApplicationVoIP, resolved.Application)
	}

	resolved = resolveOggOpusEncoderConfig(&OggOpusEncoderConfig{})
	if resolved.Application != OggOpusApplicationVoIP {
		t.Fatalf("expected empty Application to default to %q, got %q", OggOpusApplicationVoIP, resolved.Application)
	}
}

func TestOggOpusStreamEncoderBuffersUntilFrameReady(t *testing.T) {
	t.Parallel()

	encoder, err := NewOggOpusStreamEncoder(24000, 32000, nil, false)
	if err != nil {
		t.Fatalf("NewOggOpusStreamEncoder returned error: %v", err)
	}

	first, err := encoder.Encode(bytes.Repeat([]byte{0x00, 0x00}, 100), false)
	if err != nil {
		t.Fatalf("Encode returned error for partial frame: %v", err)
	}
	if len(first.Payload) != 0 {
		t.Fatalf("expected no payload for partial frame, got %d bytes", len(first.Payload))
	}

	second, err := encoder.Encode(bytes.Repeat([]byte{0x00, 0x00}, 380), true)
	if err != nil {
		t.Fatalf("Encode returned error for flushed frame: %v", err)
	}
	if !bytes.HasPrefix(second.Payload, []byte("OggS")) {
		t.Fatalf("expected Ogg payload, got %q", second.Payload[:min(4, len(second.Payload))])
	}
	if second.CompletedStream != nil {
		t.Fatal("expected completed stream to be nil when collection is disabled")
	}
}

func TestOggOpusStreamEncoderCollectsCompletedStream(t *testing.T) {
	t.Parallel()

	encoder, err := NewOggOpusStreamEncoder(24000, 32000, &OggOpusEncoderConfig{
		FrameDurationMS: 20,
		Application:     OggOpusApplicationAudio,
	}, true)
	if err != nil {
		t.Fatalf("NewOggOpusStreamEncoder returned error: %v", err)
	}

	chunk, err := encoder.Encode(bytes.Repeat([]byte{0x00, 0x00}, 480), true)
	if err != nil {
		t.Fatalf("Encode returned error: %v", err)
	}
	if !bytes.HasPrefix(chunk.Payload, []byte("OggS")) {
		t.Fatalf("expected OggS prefix, got %q", chunk.Payload[:min(4, len(chunk.Payload))])
	}
	if !bytes.Equal(chunk.Payload, chunk.CompletedStream) {
		t.Fatal("expected completed stream to equal payload for a single terminal chunk")
	}
	if !bytes.Contains(chunk.Payload, []byte("OpusHead")) {
		t.Fatal("expected payload to include OpusHead packet")
	}
	if !bytes.Contains(chunk.Payload, []byte("OpusTags")) {
		t.Fatal("expected payload to include OpusTags packet")
	}
}

func TestOggOpusStreamEncoderRejectsOddPCMInput(t *testing.T) {
	t.Parallel()

	encoder, err := NewOggOpusStreamEncoder(24000, 0, nil, false)
	if err != nil {
		t.Fatalf("NewOggOpusStreamEncoder returned error: %v", err)
	}

	_, err = encoder.Encode([]byte{0x01}, false)
	if err == nil || !strings.Contains(err.Error(), "16-bit aligned") {
		t.Fatalf("expected alignment error, got %v", err)
	}
}
