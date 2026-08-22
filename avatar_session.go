package spatiussdkgo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	message "github.com/spatius-ai/spatius-sdk-go/proto/generated"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/protobuf/proto"
)

const (
	sessionTokenPath     = "/session-tokens"
	ingressWebSocketPath = "/websocket"
)

// requestTelemetry tracks per-request telemetry state for one req_id.
type requestTelemetry struct {
	span                trace.Span
	startedAt           time.Time
	audioBytes          int64
	firstAnimationAt    time.Time
	animationFrameCount int64
}

// AvatarSession represents an active avatar session configured via SessionOptions.
type AvatarSession struct {
	config       *SessionConfig
	sessionToken string
	conn         *websocket.Conn
	currentReqID string
	lastReqID    string // tracks the most recent request ID for interrupt
	connectionID string
	audioEncoder *OggOpusStreamEncoder

	telemetryMu              sync.Mutex
	requestTelemetry         map[string]*requestTelemetry
	sessionStartedAt         time.Time
	sessionTelemetryFinished bool
}

// NewAvatarSession creates a new AvatarSession using the provided SessionOptions.
func NewAvatarSession(opts ...SessionOption) *AvatarSession {
	cfg := defaultSessionConfig()
	for _, opt := range opts {
		if opt != nil {
			opt(cfg)
		}
	}
	cfg.applyEndpointDefaults()
	return &AvatarSession{config: cfg}
}

// Config returns a copy of the session configuration.
func (s *AvatarSession) Config() SessionConfig {
	if s == nil || s.config == nil {
		return SessionConfig{}
	}
	return *s.config
}

// Init exchanges configuration credentials for a session token against the console API.
// It first resolves the ingress region (when set to "auto") via the global
// bootstrap API and composes the endpoint URLs from the result.
func (s *AvatarSession) Init(ctx context.Context) error {
	if s == nil {
		return errors.New("init avatar session: session is nil")
	}
	if s.config == nil {
		return errors.New("init avatar session: session config is nil")
	}

	s.ensureRegionResolved(ctx)
	setResourceContext(s.config.AppID, s.config.Region)

	span := startSpan("avatar.session.init", s.telemetryTraceAttributes())
	err := s.init(ctx)
	finishSpan(span, map[string]any{"region": s.config.Region}, err)
	return err
}

func (s *AvatarSession) init(ctx context.Context) error {
	cfg := s.config
	if cfg.APIKey == "" {
		return errors.New("init avatar session: missing API key")
	}
	if cfg.ConsoleEndpointURL == "" {
		return errors.New("init avatar session: missing console endpoint URL")
	}
	if cfg.ExpireAt.IsZero() {
		return errors.New("init avatar session: missing expireAt")
	}

	endpoint := strings.TrimRight(cfg.ConsoleEndpointURL, "/") + sessionTokenPath

	payload := sessionTokenRequest{
		ExpireAt: cfg.ExpireAt.UTC().Unix(),
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("init avatar session: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("init avatar session: create request: %w", err)
	}
	req.Header.Set("X-Api-Key", cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	requestStartedAt := time.Now()
	serverAddress := ""
	if parsed, parseErr := url.Parse(endpoint); parseErr == nil {
		serverAddress = parsed.Hostname()
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		recordHTTPClientDuration(sessionTokenPath, http.MethodPost,
			float64(time.Since(requestStartedAt).Milliseconds()), 0, serverAddress)
		return fmt.Errorf("init avatar session: request session token: %w", err)
	}
	defer resp.Body.Close() // nolint:errcheck

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		recordHTTPClientDuration(sessionTokenPath, http.MethodPost,
			float64(time.Since(requestStartedAt).Milliseconds()), resp.StatusCode, serverAddress)
		return fmt.Errorf("init avatar session: read response: %w", err)
	}
	recordHTTPClientDuration(sessionTokenPath, http.MethodPost,
		float64(time.Since(requestStartedAt).Milliseconds()), resp.StatusCode, serverAddress)

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("init avatar session: request failed with status %d", resp.StatusCode)
	}

	var tokenResp sessionTokenResponse
	if err := json.Unmarshal(respBody, &tokenResp); err != nil {
		return fmt.Errorf("init avatar session: decode response: %w", err)
	}
	if len(tokenResp.Errors) > 0 {
		return fmt.Errorf("init avatar session: %s", formatSessionTokenError(resp.StatusCode, &tokenResp))
	}
	if tokenResp.SessionToken == "" {
		return errors.New("init avatar session: empty session token in response")
	}

	s.sessionToken = tokenResp.SessionToken
	return nil
}

// Start establishes WebSocket connection to the ingress endpoint and performs v2 handshake.
// Returns the connection ID for tracking this session.
func (s *AvatarSession) Start(ctx context.Context) (string, error) {
	span := startSpan("avatar.session.start", s.telemetryTraceAttributes())
	startedAt := time.Now()

	connectionID, err := s.start(ctx)
	if err != nil {
		recordMetric("avatar.session.start.duration",
			float64(time.Since(startedAt).Milliseconds()),
			s.telemetryMetricAttributes(boolPtr(false)))
		finishSpan(span, nil, err)
		return "", err
	}

	s.sessionStartedAt = time.Now()
	s.sessionTelemetryFinished = false
	recordMetric("avatar.session.start.duration",
		float64(time.Since(startedAt).Milliseconds()),
		s.telemetryMetricAttributes(boolPtr(true)))
	finishSpan(span, map[string]any{"connection_id": connectionID}, nil)
	return connectionID, nil
}

func (s *AvatarSession) start(ctx context.Context) (string, error) {
	if s == nil {
		return "", errors.New("start avatar session: session is nil")
	}
	if s.config == nil {
		return "", errors.New("start avatar session: session config is nil")
	}
	if s.conn != nil {
		return "", errors.New("start avatar session: session already started")
	}
	if s.sessionToken == "" {
		return "", errors.New("start avatar session: session not initialized")
	}

	cfg := s.config
	if cfg.IngressEndpointURL == "" {
		return "", errors.New("start avatar session: missing ingress endpoint URL")
	}
	if cfg.AvatarID == "" {
		return "", errors.New("start avatar session: missing avatar ID")
	}
	if cfg.AppID == "" {
		return "", errors.New("start avatar session: missing app ID")
	}

	endpoint := strings.TrimRight(cfg.IngressEndpointURL, "/") + ingressWebSocketPath

	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("start avatar session: parse ingress endpoint: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
		// already websocket scheme
	case "":
		return "", errors.New("start avatar session: ingress endpoint scheme missing")
	default:
		return "", fmt.Errorf("start avatar session: unsupported scheme %q", u.Scheme)
	}

	q := u.Query()
	q.Set("id", cfg.AvatarID)

	// v2 auth: mobile uses headers; web uses query params.
	headers := http.Header{}
	if cfg.UseQueryAuth {
		q.Set("appId", cfg.AppID)
		q.Set("sessionKey", s.sessionToken)
	} else {
		headers.Set("X-App-ID", cfg.AppID)
		headers.Set("X-Session-Key", s.sessionToken)
	}

	u.RawQuery = q.Encode()

	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, u.String(), headers)
	if err != nil {
		if resp != nil {
			// Map HTTP status to SDK error code
			if code := mapWSConnectErrorToCode(resp.StatusCode); code != nil {
				return "", NewAvatarSDKError(*code, fmt.Sprintf("WebSocket auth failed (HTTP %d)", resp.StatusCode))
			}
			if resp.Body != nil {
				defer resp.Body.Close() // nolint:errcheck
				if body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4096)); readErr == nil && len(body) > 0 {
					return "", fmt.Errorf("start avatar session: dial websocket failed with code %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
				}
			}
		}
		return "", fmt.Errorf("start avatar session: dial websocket: %w", err)
	}

	s.conn = conn

	// v2 handshake:
	// 1) client sends ClientConfigureSession
	// 2) server responds with ServerConfirmSession (connection_id) OR ServerError
	if err := s.sendClientConfigureSession(); err != nil {
		_ = conn.Close()
		s.conn = nil
		return "", err
	}

	connectionID, err := s.awaitServerConfirmSession(ctx)
	if err != nil {
		_ = conn.Close()
		s.conn = nil
		return "", err
	}

	s.connectionID = connectionID

	// Start read loop in background
	go s.readLoop(ctx)

	return connectionID, nil
}

// sendClientConfigureSession sends the v2 handshake configuration message.
func (s *AvatarSession) sendClientConfigureSession() error {
	if s.conn == nil {
		return errors.New("websocket connection is not established")
	}

	clientConfig := &message.ClientConfigureSession{
		SampleRate:           int32(s.config.SampleRate),
		Bitrate:              int32(s.config.Bitrate),
		AudioFormat:          protoAudioFormat(s.config.AudioFormat),
		TransportCompression: message.TransportCompression_TRANSPORT_COMPRESSION_NONE,
	}

	if len(s.config.ExtraParams) > 0 {
		clientConfig.ExtraParams = s.config.ExtraParams
	}

	// Add LiveKit egress configuration if provided
	if s.config.LiveKitEgress != nil {
		clientConfig.EgressType = message.EgressType_EGRESS_TYPE_LIVEKIT
		clientConfig.LivekitEgress = &message.LiveKitEgressConfig{
			Url:             s.config.LiveKitEgress.URL,
			ApiKey:          s.config.LiveKitEgress.APIKey,
			ApiSecret:       s.config.LiveKitEgress.APISecret,
			ApiToken:        s.config.LiveKitEgress.APIToken,
			RoomName:        s.config.LiveKitEgress.RoomName,
			PublisherId:     s.config.LiveKitEgress.PublisherID,
			ExtraAttributes: s.config.LiveKitEgress.ExtraAttributes,
			IdleTimeout:     s.config.LiveKitEgress.IdleTimeout,
		}
	}

	// Add Agora egress configuration if provided
	if s.config.AgoraEgress != nil {
		clientConfig.EgressType = message.EgressType_EGRESS_TYPE_AGORA
		clientConfig.AgoraEgress = &message.AgoraEgressConfig{
			ChannelName: s.config.AgoraEgress.ChannelName,
			Token:       s.config.AgoraEgress.Token,
			Uid:         s.config.AgoraEgress.UID,
			PublisherId: s.config.AgoraEgress.PublisherID,
		}
	}

	msg := &message.Message{
		Type: message.MessageType_MESSAGE_CLIENT_CONFIGURE_SESSION,
		Data: &message.Message_ClientConfigureSession{
			ClientConfigureSession: clientConfig,
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return fmt.Errorf("start avatar session: marshal configure session message: %w", err)
	}

	if err := s.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		return fmt.Errorf("start avatar session: send configure session message: %w", err)
	}

	return nil
}

// awaitServerConfirmSession waits for the server's handshake response.
func (s *AvatarSession) awaitServerConfirmSession(ctx context.Context) (string, error) {
	if s.conn == nil {
		return "", errors.New("websocket connection is not established")
	}

	// Set read deadline based on context
	if deadline, ok := ctx.Deadline(); ok {
		if err := s.conn.SetReadDeadline(deadline); err != nil {
			return "", fmt.Errorf("start avatar session: set read deadline: %w", err)
		}
		defer s.conn.SetReadDeadline(time.Time{}) // nolint:errcheck
	}

	messageType, payload, err := s.conn.ReadMessage()
	if err != nil {
		return "", fmt.Errorf("start avatar session: failed during websocket handshake: %w", err)
	}

	if messageType != websocket.BinaryMessage {
		return "", NewAvatarSDKError(
			ErrorCodeProtocolError,
			"failed during websocket handshake: expected binary protobuf message",
		)
	}

	var envelope message.Message
	if err := proto.Unmarshal(payload, &envelope); err != nil {
		return "", NewAvatarSDKError(
			ErrorCodeProtocolError,
			fmt.Sprintf("failed during websocket handshake: invalid protobuf payload: %v", err),
		)
	}

	switch envelope.GetType() {
	case message.MessageType_MESSAGE_SERVER_CONFIRM_SESSION:
		confirm := envelope.GetServerConfirmSession()
		if confirm == nil || confirm.GetConnectionId() == "" {
			return "", NewAvatarSDKError(
				ErrorCodeProtocolError,
				"handshake succeeded but server_confirm_session.connection_id is empty",
			)
		}
		return confirm.GetConnectionId(), nil

	case message.MessageType_MESSAGE_SERVER_ERROR:
		serverErr := envelope.GetServerError()
		if serverErr == nil {
			return "", NewAvatarSDKError(ErrorCodeProtocolError, "server error during handshake: missing payload")
		}
		return "", newServerAvatarSDKError(
			"websocket_handshake",
			serverErr.GetCode(),
			serverErr.GetMessage(),
			serverErr.GetConnectionId(),
			serverErr.GetReqId(),
		)

	default:
		return "", fmt.Errorf("start avatar session: unexpected message during handshake: type=%v", envelope.GetType())
	}
}

// SendAudio sends audio data to the server.
// Audio must match the session's negotiated format unless the internal Ogg Opus encoder is enabled.
func (s *AvatarSession) SendAudio(audio []byte, end bool) (string, error) {
	if s.conn == nil {
		return "", errors.New("send audio: websocket connection is not established")
	}

	var err error
	if s.currentReqID == "" {
		s.currentReqID, err = GenerateLogID()
		if err != nil {
			return "", fmt.Errorf("send audio: generate request id: %w", err)
		}
		s.lastReqID = s.currentReqID
	}

	reqID := s.currentReqID
	payload := audio
	var encodedStream []byte

	useInternalEncoder := s.usesInternalOggOpusEncoder()
	if useInternalEncoder {
		encoder, err := s.getOrCreateAudioEncoder()
		if err != nil {
			return "", fmt.Errorf("send audio: %w", err)
		}

		encodedChunk, err := encoder.Encode(audio, end)
		if err != nil {
			return "", fmt.Errorf("send audio: %w", err)
		}

		payload = encodedChunk.Payload
		encodedStream = encodedChunk.CompletedStream
	}

	if useInternalEncoder && len(payload) == 0 && !end {
		return reqID, nil
	}

	// Create a request span immediately before the first transmitted audio
	// chunk, then propagate its W3C context to the backend once per req_id.
	traceContext := s.startRequestTelemetry(reqID)

	audioInput := &message.ClientAudioInput{
		ReqId: reqID,
		Audio: payload,
		End:   end,
	}
	if traceparent := traceContext["traceparent"]; traceparent != "" || traceContext["tracestate"] != "" {
		audioInput.TraceContext = &message.TraceContext{
			Traceparent: traceparent,
			Tracestate:  traceContext["tracestate"],
		}
	}

	msg := &message.Message{
		Type: message.MessageType_MESSAGE_CLIENT_AUDIO_INPUT,
		Data: &message.Message_ClientAudioInput{
			ClientAudioInput: audioInput,
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("send audio: marshal message: %w", err)
	}

	if err := s.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		sendErr := fmt.Errorf("send audio: write message: %w", err)
		s.finishRequestTelemetry(reqID, "send_error", sendErr)
		return "", sendErr
	}

	s.recordAudioSentTelemetry(reqID, int64(len(payload)), end)

	if len(encodedStream) > 0 {
		s.notifyEncodedAudio(reqID, encodedStream)
	}

	if end {
		s.currentReqID = ""
		s.audioEncoder = nil
	}

	return reqID, nil
}

// Interrupt sends an interrupt signal to stop the current audio processing.
// Returns the request ID that was interrupted, or empty string if no request was active.
func (s *AvatarSession) Interrupt() (string, error) {
	if s.conn == nil {
		return "", errors.New("interrupt: websocket connection is not established")
	}

	// Use lastReqID which tracks the most recent request, even after end=true
	reqID := s.lastReqID
	if reqID == "" {
		return "", errors.New("interrupt: no request to interrupt")
	}

	msg := &message.Message{
		Type: message.MessageType_MESSAGE_CLIENT_INTERRUPT,
		Data: &message.Message_ClientInterrupt{
			ClientInterrupt: &message.ClientInterrupt{
				ReqId: reqID,
			},
		},
	}

	data, err := proto.Marshal(msg)
	if err != nil {
		return "", fmt.Errorf("interrupt: marshal message: %w", err)
	}

	if err := s.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		interruptErr := fmt.Errorf("interrupt: write message: %w", err)
		s.finishRequestTelemetry(reqID, "interrupt_error", interruptErr)
		return "", interruptErr
	}

	s.finishRequestTelemetry(reqID, "interrupted", nil)

	// Clear current request ID so next SendAudio creates a new one
	s.currentReqID = ""

	return reqID, nil
}

// Close closes the WebSocket connection and cleans up resources.
func (s *AvatarSession) Close() error {
	if s == nil {
		return nil
	}

	s.finishAllRequestTelemetry("session_closed")

	if !s.sessionStartedAt.IsZero() && !s.sessionTelemetryFinished {
		recordMetric("avatar.session.duration",
			float64(time.Since(s.sessionStartedAt).Milliseconds()),
			s.telemetryMetricAttributes(boolPtr(true)))
		s.sessionTelemetryFinished = true
	}

	if s.conn != nil {
		err := s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		if err != nil {
			_ = s.conn.Close()
			s.conn = nil
			return fmt.Errorf("close avatar session: send close message: %w", err)
		}
		err = s.conn.Close()
		if err != nil {
			s.conn = nil
			return fmt.Errorf("close avatar session: close connection: %w", err)
		}
		s.conn = nil
	}
	if s.config != nil && s.config.OnClose != nil {
		go s.config.OnClose()
	}
	return nil
}

type sessionTokenRequest struct {
	ExpireAt     int64  `json:"expireAt"`
	ModelVersion string `json:"modelVersion,omitempty"`
}

type sessionTokenResponse struct {
	SessionToken string `json:"sessionToken"`
	Errors       []struct {
		ID     string `json:"id"`
		Status int    `json:"status"`
		Code   string `json:"code"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	} `json:"errors"`
}

func formatSessionTokenError(status int, resp *sessionTokenResponse) string {
	// format resp.Errors[0] as "Error <status> (<code>): <title> - <detail>"
	if len(resp.Errors) == 0 {
		return fmt.Sprintf("unknown error with status %d", status)
	}
	err := resp.Errors[0]
	return fmt.Sprintf("Error %d (%s): %s - %s", err.Status, err.Code, err.Title, err.Detail)
}

type callbackDispatcher struct {
	mu     sync.Mutex
	cond   *sync.Cond
	queue  []func()
	closed bool
}

func newCallbackDispatcher() *callbackDispatcher {
	d := &callbackDispatcher{}
	d.cond = sync.NewCond(&d.mu)
	go d.run()
	return d
}

func (d *callbackDispatcher) dispatch(callback func()) {
	if d == nil || callback == nil {
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	d.queue = append(d.queue, callback)
	d.cond.Signal()
}

func (d *callbackDispatcher) stop() {
	if d == nil {
		return
	}

	d.mu.Lock()
	d.closed = true
	d.cond.Signal()
	d.mu.Unlock()
}

func (d *callbackDispatcher) run() {
	for {
		d.mu.Lock()
		for len(d.queue) == 0 && !d.closed {
			d.cond.Wait()
		}
		if len(d.queue) == 0 && d.closed {
			d.mu.Unlock()
			return
		}
		callback := d.queue[0]
		d.queue[0] = nil
		d.queue = d.queue[1:]
		d.mu.Unlock()

		callback()
	}
}

func (s *AvatarSession) readLoop(ctx context.Context) {
	if s == nil {
		return
	}

	conn := s.conn
	if conn == nil {
		return
	}

	cfg := s.config
	callbacks := newCallbackDispatcher()
	defer callbacks.stop()

	for {
		if ctx != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
		}

		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			if ctx != nil && ctx.Err() != nil {
				return
			}

			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return
			}

			if cfg != nil && cfg.OnError != nil {
				asyncErr := fmt.Errorf("avatar session read loop: read message: %w", err)
				callbacks.dispatch(func() { cfg.OnError(asyncErr) })
			}

			_ = s.Close()
			return
		}

		if messageType != websocket.BinaryMessage {
			continue
		}

		var envelope message.Message
		if err := proto.Unmarshal(payload, &envelope); err != nil {
			if cfg != nil && cfg.OnError != nil {
				asyncErr := fmt.Errorf("avatar session read loop: decode message: %w", err)
				callbacks.dispatch(func() { cfg.OnError(asyncErr) })
			}
			continue
		}

		switch envelope.GetType() {
		case message.MessageType_MESSAGE_SERVER_RESPONSE_ANIMATION:
			anim := envelope.GetServerResponseAnimation()
			last := anim != nil && anim.GetEnd()
			if anim != nil {
				s.recordAnimationTelemetry(anim.GetReqId(), last)
			}
			if cfg != nil && cfg.TransportFrames != nil {
				frame := append([]byte(nil), payload...)
				callbacks.dispatch(func() { cfg.TransportFrames(frame, last) })
			}
		case message.MessageType_MESSAGE_SERVER_ERROR:
			serverErr := envelope.GetServerError()
			if serverErr == nil {
				if cfg != nil && cfg.OnError != nil {
					callbackErr := errors.New("avatar session read loop: error message missing payload")
					callbacks.dispatch(func() { cfg.OnError(callbackErr) })
				}
				continue
			}
			report := newServerAvatarSDKError(
				"runtime",
				serverErr.GetCode(),
				serverErr.GetMessage(),
				serverErr.GetConnectionId(),
				serverErr.GetReqId(),
			)
			s.finishRequestTelemetry(serverErr.GetReqId(), "server_error", report)
			if cfg != nil && cfg.OnError != nil {
				callbacks.dispatch(func() { cfg.OnError(report) })
			}
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}

// telemetryEgressType returns the egress delivery mode used in telemetry attributes.
func (s *AvatarSession) telemetryEgressType() string {
	if s == nil || s.config == nil {
		return "websocket"
	}
	if s.config.LiveKitEgress != nil {
		return "livekit"
	}
	if s.config.AgoraEgress != nil {
		return "agora"
	}
	return "websocket"
}

// telemetryMetricAttributes returns the shared attributes for session metrics.
func (s *AvatarSession) telemetryMetricAttributes(success *bool) map[string]any {
	attrs := map[string]any{
		"region":       "",
		"audio_format": "",
		"egress_type":  s.telemetryEgressType(),
	}
	if s != nil && s.config != nil {
		attrs["region"] = s.config.Region
		attrs["audio_format"] = string(s.config.AudioFormat)
	}
	if success != nil {
		attrs["success"] = *success
	}
	return attrs
}

// telemetryTraceAttributes returns the shared attributes for session spans.
func (s *AvatarSession) telemetryTraceAttributes() map[string]any {
	attrs := s.telemetryMetricAttributes(nil)
	attrs["app_id"] = ""
	attrs["avatar_id"] = ""
	if s != nil && s.config != nil {
		attrs["app_id"] = s.config.AppID
		attrs["avatar_id"] = s.config.AvatarID
	}
	if s != nil && s.connectionID != "" {
		attrs["connection_id"] = s.connectionID
	}
	return attrs
}

// startRequestTelemetry starts a span for a req_id and returns its W3C trace
// context for the first audio message. It returns nil when no context should
// be attached.
func (s *AvatarSession) startRequestTelemetry(reqID string) map[string]string {
	if s == nil {
		return nil
	}

	s.telemetryMu.Lock()
	_, exists := s.requestTelemetry[reqID]
	s.telemetryMu.Unlock()
	if exists {
		return nil
	}

	attrs := s.telemetryTraceAttributes()
	attrs["req_id"] = reqID
	span := startSpan("driven.request", attrs)
	if span == nil && !telemetryEnabled() {
		return nil
	}

	s.telemetryMu.Lock()
	if s.requestTelemetry == nil {
		s.requestTelemetry = map[string]*requestTelemetry{}
	}
	s.requestTelemetry[reqID] = &requestTelemetry{span: span, startedAt: time.Now()}
	s.telemetryMu.Unlock()

	return injectTraceContext(span)
}

// finishRequestTelemetry records final request metrics and ends the span for a req_id.
func (s *AvatarSession) finishRequestTelemetry(reqID, endReason string, err error) {
	if s == nil || reqID == "" {
		return
	}

	s.telemetryMu.Lock()
	state := s.requestTelemetry[reqID]
	delete(s.requestTelemetry, reqID)
	s.telemetryMu.Unlock()
	if state == nil {
		return
	}

	durationMS := float64(time.Since(state.startedAt).Milliseconds())
	attrs := s.telemetryMetricAttributes(nil)
	attrs["end_reason"] = endReason
	recordMetric("avatar.request.duration", durationMS, attrs)
	recordMetric("avatar.request.audio_bytes", float64(state.audioBytes), attrs)
	if !state.firstAnimationAt.IsZero() {
		recordMetric("avatar.request.first_animation_latency",
			float64(state.firstAnimationAt.Sub(state.startedAt).Milliseconds()), attrs)
	}
	finishSpan(state.span, map[string]any{
		"req_id":                reqID,
		"audio_bytes":           state.audioBytes,
		"animation_frame_count": state.animationFrameCount,
		"end_reason":            endReason,
	}, err)
}

// finishAllRequestTelemetry finishes every in-flight request telemetry state.
func (s *AvatarSession) finishAllRequestTelemetry(endReason string) {
	if s == nil {
		return
	}
	s.telemetryMu.Lock()
	reqIDs := make([]string, 0, len(s.requestTelemetry))
	for reqID := range s.requestTelemetry {
		reqIDs = append(reqIDs, reqID)
	}
	s.telemetryMu.Unlock()
	for _, reqID := range reqIDs {
		s.finishRequestTelemetry(reqID, endReason, nil)
	}
}

// recordAudioSentTelemetry accumulates sent audio bytes for a req_id.
func (s *AvatarSession) recordAudioSentTelemetry(reqID string, payloadBytes int64, end bool) {
	if s == nil {
		return
	}
	s.telemetryMu.Lock()
	state := s.requestTelemetry[reqID]
	if state != nil {
		state.audioBytes += payloadBytes
	}
	s.telemetryMu.Unlock()
	if state != nil && end {
		addSpanEvent(state.span, "audio.input.complete", nil)
	}
}

// recordAnimationTelemetry tracks animation frames for a req_id and finishes
// request telemetry when the final frame arrives.
func (s *AvatarSession) recordAnimationTelemetry(reqID string, isLast bool) {
	if s == nil || reqID == "" {
		return
	}

	s.telemetryMu.Lock()
	state := s.requestTelemetry[reqID]
	var span trace.Span
	firstFrame := false
	if state != nil {
		state.animationFrameCount++
		firstFrame = state.animationFrameCount == 1
		if state.firstAnimationAt.IsZero() {
			state.firstAnimationAt = time.Now()
		}
		span = state.span
	}
	s.telemetryMu.Unlock()

	if state == nil {
		return
	}
	if firstFrame {
		addSpanEvent(span, "animation.first_frame", nil)
	}
	if isLast {
		s.finishRequestTelemetry(reqID, "animation_end", nil)
	}
}

func protoAudioFormat(audioFormat AudioFormat) message.AudioFormat {
	switch audioFormat {
	case AudioFormatOggOpus:
		return message.AudioFormat_AUDIO_FORMAT_OGG_OPUS
	case "", AudioFormatPCMS16LE:
		return message.AudioFormat_AUDIO_FORMAT_PCM_S16LE
	default:
		return message.AudioFormat_AUDIO_FORMAT_PCM_S16LE
	}
}

func (s *AvatarSession) usesInternalOggOpusEncoder() bool {
	return s != nil &&
		s.config != nil &&
		s.config.AudioFormat == AudioFormatOggOpus &&
		s.config.OggOpusEncoder != nil
}

func (s *AvatarSession) getOrCreateAudioEncoder() (*OggOpusStreamEncoder, error) {
	if s.audioEncoder != nil {
		return s.audioEncoder, nil
	}

	encoder, err := NewOggOpusStreamEncoder(
		s.config.SampleRate,
		s.config.Bitrate,
		s.config.OggOpusEncoder,
		s.config.OnEncodedAudio != nil,
	)
	if err != nil {
		return nil, err
	}

	s.audioEncoder = encoder
	return s.audioEncoder, nil
}

func (s *AvatarSession) notifyEncodedAudio(reqID string, encodedAudio []byte) {
	if s == nil || s.config == nil || s.config.OnEncodedAudio == nil {
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			log.Printf("spatiussdkgo: on encoded audio callback panicked: %v", recovered)
		}
	}()

	s.config.OnEncodedAudio(reqID, encodedAudio)
}
