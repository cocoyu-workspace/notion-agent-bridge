package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const agentRetryNoticePrefix = "[bridge:auto-retry]"

type agentRetriedError struct {
	Cause  error
	Traces []string
}

func (e *agentRetriedError) Error() string {
	if e == nil || e.Cause == nil {
		return ""
	}
	return e.Cause.Error()
}

func (e *agentRetriedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func wrapAgentRetryTraces(err error, traces []string) error {
	if err == nil || len(traces) == 0 {
		return err
	}
	return &agentRetriedError{Cause: err, Traces: append([]string(nil), traces...)}
}

func formatAgentRetryNotice(traces []string) string {
	if len(traces) == 0 {
		return ""
	}
	return fmt.Sprintf("%s recovered after %d auto-retry step(s) (%s)", agentRetryNoticePrefix, len(traces), strings.Join(traces, " -> "))
}

func stripAgentRetryNotice(text string) string {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, agentRetryNoticePrefix) {
		return text
	}
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		return strings.TrimLeft(trimmed[idx+1:], "\r\n")
	}
	return ""
}

// Late-reply recovery (issue #17).
//
// A Notion Agent turn keeps running remotely when the local wait ends (deadline,
// dropped connection, lost acknowledgement, client gone). The binding store
// already pins thread / message / event IDs, so an identical request polls or
// replays the same thread without create/send. This file adds:
//
//   - agentRecoverableError: marks such failures as "remote may still be running,
//     retry the identical request" and keeps them distinguishable from real failures;
//   - classifyAgentFailure: one place that maps errors to a structured, credential
//     free client error (kind, HTTP status, Anthropic error type, retry hint);
//   - anthropicKeepalive: legal Anthropic SSE keepalive (message_start + ping only)
//     while the remote turn is running; no answer or tool use is ever fabricated;
//   - bounded cursor paging of the transcript (agent_pagination.go).

var (
	agentKeepaliveDelay    = 5 * time.Second  // wait this long before committing to an SSE response
	agentKeepaliveInterval = 10 * time.Second // ping period afterwards
	// agentUnconfirmedGrace is how long an interrupted, unconfirmed send must
	// keep showing "not received" before the edited turn is sent in its place.
	agentUnconfirmedGrace = 3 * time.Second
	// agentPollHedgeDelay: Notion has windows in which single getThreadTranscript
	// calls stall for 10-20s while a parallel call answers at once. Large threads
	// (200KB+ with thinking blocks) normally take 2-3.5s per poll, so wait 5s
	// before hedging and rotating the HTTP/2 transport.
	agentPollHedgeDelay       = 5 * time.Second
	agentPollMaxInFlight      = 3
	agentPollAttemptTimeout   = 15 * time.Second
	agentSubmitAttemptTimeout = 12 * time.Second
	agentSubmitMaxAttempts    = 5
	agentSubmitRetryDelay     = 1 * time.Second
	agentUploadMaxAttempts    = 3
	agentInferenceMaxAttempts = 5
	agentInferenceRetryDelay  = 1 * time.Second
	agentDeadlineMaxRetries   = 1
)

func agentBackoffSleep(ctx context.Context, gone <-chan struct{}, base time.Duration, attempt int) error {
	if base <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gone:
			return errAgentClientGone
		default:
			return nil
		}
	}
	shift := attempt - 1
	if shift < 0 {
		shift = 0
	} else if shift > 3 {
		shift = 3
	}
	timer := time.NewTimer(base * time.Duration(1<<shift))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-gone:
		return errAgentClientGone
	case <-timer.C:
		return nil
	}
}

func isAgentInferenceRetryable(err error, attempt int, deadlineRetries *int) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrQuotaExhausted) || errors.Is(err, ErrToolBridgeMalformed) || errors.Is(err, ErrEmptyResponse) || errors.Is(err, errAgentClientGone) {
		return false
	}
	if _, ok := classifyUpstreamConfig(err); ok {
		return false
	}
	var httpErr *agentHTTPError
	if errors.As(err, &httpErr) && (httpErr.Status == http.StatusPaymentRequired || httpErr.Status == http.StatusForbidden || httpErr.crossCell()) {
		return false
	}
	var terr *agentTranscriptError
	if errors.As(err, &terr) {
		// An unreadable transcript page (transcript_format) keeps the binding so
		// the identical request polls again; a failed turn discovered while probing
		// an earlier unconfirmed submit (attempt > 1) has already rolled back the
		// binding and assigned a fresh clientMessageID for the next attempt.
		return terr.kind == "transcript_format" || attempt > 1
	}
	var rec *agentRecoverableError
	if errors.As(err, &rec) {
		switch rec.Class {
		case "client_gone", "correction_pending":
			return false
		case "deadline":
			if deadlineRetries != nil && *deadlineRetries < agentDeadlineMaxRetries && AppConfig != nil && AppConfig.Timeouts.InferenceTimeout >= 60 {
				*deadlineRetries++
				return true
			}
			return false
		default:
			return true
		}
	}
	var bind *agentBindingError
	if errors.As(err, &bind) {
		switch bind.Kind {
		case "busy":
			return true
		case "rejected":
			return errors.As(bind.Cause, &httpErr) && ((httpErr.Status == http.StatusBadRequest && httpErr.Code == "UserValidationError") || httpErr.Status == http.StatusTooManyRequests)
		default:
			return false
		}
	}
	if errors.As(err, &httpErr) {
		if httpErr.Status == http.StatusBadRequest && httpErr.Code == "UserValidationError" {
			return true
		}
		return !httpErr.definitiveRejection() && !httpErr.crossCell()
	}
	return false
}

// hedgedPoll runs a read-only fetch, starting another identical attempt each
// agentPollHedgeDelay while none has finished (at most agentPollMaxInFlight).
// The first finished attempt's result is returned unchanged; nothing is ever
// abandoned without a replacement, so a slow but working poll still counts.
func hedgedPoll[T any](ctx context.Context, fetch func(context.Context) (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, agentPollMaxInFlight)
	start := time.Now()
	launch := func() {
		go func() {
			actx := hctx
			var acancel context.CancelFunc
			if agentPollAttemptTimeout > 0 {
				actx, acancel = context.WithTimeout(hctx, agentPollAttemptTimeout)
				defer acancel()
			}
			v, err := fetch(actx)
			results <- result{v, err}
		}()
	}
	launch()
	inFlight := 1
	finished := 0
	var lastErr error
	timer := time.NewTimer(agentPollHedgeDelay)
	defer timer.Stop()
	for {
		select {
		case r := <-results:
			finished++
			if r.err == nil || inFlight == 1 || hctx.Err() != nil || finished == inFlight && inFlight == agentPollMaxInFlight {
				if inFlight > 1 && r.err == nil {
					log.Printf("[agent-thread] slow transcript poll: answered after %.1fs with %d parallel request(s)", time.Since(start).Seconds(), inFlight)
				}
				return r.v, r.err
			}
			lastErr = r.err
			if finished == inFlight && inFlight < agentPollMaxInFlight {
				RebuildChromeTransport()
				launch()
				inFlight++
				timer.Reset(agentPollHedgeDelay)
			}
		case <-timer.C:
			if inFlight < agentPollMaxInFlight {
				RebuildChromeTransport()
				launch()
				inFlight++
				timer.Reset(agentPollHedgeDelay)
			} else if finished == inFlight {
				var zero T
				return zero, lastErr
			}
		}
	}
}

// agentTranscriptPageLimit is the `limit` the bridge has always requested. It is
// not a cap on entities (see agent_pagination.go): longer threads are read by
// following backward_cursor, never by raising the limit.
const agentTranscriptPageLimit = 100

// errAgentClientGone ends the local wait because the HTTP client disconnected.
var errAgentClientGone = errors.New("client disconnected while waiting")

// agentThreadHTTPClient is the HTTP client factory for the Agent transport; tests replace it.
var agentThreadHTTPClient = func(timeout time.Duration) *http.Client { return getChromeHTTPClient(timeout) }

// ---- error types -----------------------------------------------------------

// agentRecoverableError wraps a failure after which the remote turn may still be
// running or already finished. Retrying the identical request recovers the reply
// from the same thread without a second create/send.
type agentRecoverableError struct {
	Class    string // deadline | network | http_<status> | client_gone | unknown_outcome
	ThreadID string
	Cause    error
}

func (e *agentRecoverableError) Error() string {
	return fmt.Sprintf("Notion Agent turn may still be running (reason=%s, thread=%s): %v", e.Class, shortID(e.ThreadID), e.Cause)
}
func (e *agentRecoverableError) Unwrap() error { return e.Cause }

// agentTranscriptIncompleteError: the transcript could not be read completely
// within the bounded page size. The remote turn is not known to have failed, so
// the binding is left untouched and no partial/old text is ever returned.
type agentTranscriptIncompleteError struct{ msg string }

func (e *agentTranscriptIncompleteError) Error() string { return e.msg }

// agentModelError: the requested model alias is not a verified Agent model
// (nothing was sent to Notion).
type agentModelError struct{ model string }

func (e *agentModelError) Error() string {
	return fmt.Sprintf("unsupported Notion Agent model %q; use auto, opus-5.5, sonnet-5.5, or gpt-6.1-sol", e.model)
}

func transcriptErrKind(kind, format string, args ...interface{}) error {
	return &agentTranscriptError{msg: fmt.Sprintf(format, args...), kind: kind}
}

// transientClass names why a store-backed turn could not be followed to its end,
// or "" when the error is definitive (a retry cannot help).
func transientClass(err error) string {
	var rec *agentRecoverableError
	if errors.As(err, &rec) {
		return rec.Class
	}
	var bind *agentBindingError
	var terr *agentTranscriptError
	var inc *agentTranscriptIncompleteError
	var merr *agentModelError
	if errors.As(err, &bind) || errors.As(err, &terr) || errors.As(err, &inc) || errors.As(err, &merr) || errors.Is(err, ErrToolBridgeMalformed) || errors.Is(err, ErrEmptyResponse) || errors.Is(err, ErrQuotaExhausted) {
		return ""
	}
	var httpErr *agentHTTPError
	if errors.As(err, &httpErr) {
		if httpErr.definitiveRejection() {
			return ""
		}
		return fmt.Sprintf("http_%d", httpErr.Status)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline"
	}
	var netErr net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &netErr) {
		return "network"
	}
	return "unknown_outcome"
}

// markAgentRecoverable wraps transient failures of a store-backed turn.
func markAgentRecoverable(turn *agentTurn, err error) error {
	if err == nil || turn == nil || turn.store == nil || turn.rec == nil {
		return err
	}
	class := transientClass(err)
	if class == "" {
		return err
	}
	var rec *agentRecoverableError
	if errors.As(err, &rec) {
		return err
	}
	return &agentRecoverableError{Class: class, ThreadID: turn.threadID, Cause: err}
}

// ---- classification --------------------------------------------------------

type agentFailure struct {
	Kind        string // pending | failed | model_unavailable | rejected | transcript_format | transcript_incomplete | busy | diverged | quota | internal
	Class       string
	Pending     bool
	Status      int
	ErrType     string // Anthropic error.type
	ShouldRetry bool   // x-should-retry header: true only when the identical request recovers
	Message     string
}

// classifyAgentFailure maps a transport error to a structured client error.
// Messages carry kinds, HTTP statuses and short thread IDs only: no URLs, cookies
// or request/response bodies.
func classifyAgentFailure(err error) agentFailure {
	f := classifyAgentFailureBase(err)
	var retried *agentRetriedError
	if errors.As(err, &retried) && len(retried.Traces) > 0 {
		f.Message = fmt.Sprintf("%s [auto_retries=%d: %s]", f.Message, len(retried.Traces), strings.Join(retried.Traces, " -> "))
	}
	return f
}

func classifyAgentFailureBase(err error) agentFailure {
	var rec *agentRecoverableError
	var bind *agentBindingError
	var terr *agentTranscriptError
	var inc *agentTranscriptIncompleteError
	var merr *agentModelError
	var httpErr *agentHTTPError
	var malformed *toolBridgeMalformedError
	if !errors.As(err, &malformed) {
		// Explicit upstream conditions (quota, wrong API domain) win over the generic
		// pending/rejected classes below.
		if f, ok := classifyUpstreamConfig(err); ok {
			return f
		}
	}
	switch {
	case errors.As(err, &malformed):
		return agentFailure{Kind: "malformed_tool_call", Status: http.StatusBadGateway, ErrType: "api_error",
			Message: fmt.Sprintf("[bridge:malformed_tool_call] the Notion reply looked like a local tool request but could not be validated (reason=%s). Nothing was executed, nothing was resent, and the text was not returned as an answer.", malformed.Reason)}
	case errors.As(err, &rec):
		return agentFailure{Kind: "pending", Class: rec.Class, Pending: true, Status: http.StatusGatewayTimeout, ErrType: "api_error", ShouldRetry: true,
			Message: fmt.Sprintf("[bridge:pending] Notion is still working on this request or its reply was not collected yet (reason=%s, thread=%s). Nothing was resent. Retry the identical request to collect the reply.", rec.Class, shortID(rec.ThreadID))}
	case errors.As(err, &merr):
		return agentFailure{Kind: "model_unavailable", Status: http.StatusBadRequest, ErrType: "invalid_request_error",
			Message: fmt.Sprintf("[bridge:model_unavailable] model %q is not a supported Notion Agent model (use auto, opus-5.5, sonnet-5.5 or gpt-6.1-sol). Nothing was sent to Notion.", merr.model)}
	case errors.As(err, &inc):
		return agentFailure{Kind: "transcript_incomplete", Status: http.StatusBadGateway, ErrType: "api_error",
			Message: "[bridge:transcript_incomplete] the Notion transcript could not be read completely (page chain bounded or a page was unreadable); refusing to return partial or older output. The remote turn was not cancelled and nothing was resent."}
	case errors.As(err, &terr):
		if terr.kind == "transcript_format" {
			return agentFailure{Kind: "transcript_format", Status: http.StatusBadGateway, ErrType: "api_error",
				Message: "[bridge:transcript_format] unexpected Notion transcript format: " + terr.msg + ". The remote turn was not marked failed and nothing was resent; the identical request polls it again."}
		}
		return agentFailure{Kind: "failed", Status: http.StatusBadGateway, ErrType: "api_error",
			Message: "[bridge:failed] the Notion Agent turn failed (" + terr.msg + "). The remote turn ended; retrying the same request will not produce a reply."}
	case errors.Is(err, ErrEmptyResponse):
		return agentFailure{Kind: "failed", Status: http.StatusBadGateway, ErrType: "api_error",
			Message: "[bridge:failed] the Notion Agent turn finished without a reply. The remote turn ended; retrying the same request will not produce a reply."}
	case errors.As(err, &bind):
		f := agentFailure{Kind: bind.Kind, Status: http.StatusConflict, ErrType: "api_error", Message: "[bridge:" + bind.Kind + "] " + bind.Msg}
		switch bind.Kind {
		case "busy", "pending", "rejected":
			f.ShouldRetry = true
		case "diverged":
			f.Status, f.ErrType = http.StatusBadRequest, "invalid_request_error"
		case "too_large":
			f.Status, f.ErrType = http.StatusRequestEntityTooLarge, "request_too_large"
		default:
			f.Status = http.StatusBadGateway
		}
		return f
	case errors.As(err, &httpErr):
		retry := (httpErr.Endpoint == "createAgentThread" || httpErr.Endpoint == "sendEventToAgentThread") &&
			httpErr.Status == http.StatusBadRequest && httpErr.Code == "UserValidationError"
		return agentFailure{Kind: "rejected", Status: http.StatusBadGateway, ErrType: "api_error", ShouldRetry: retry,
			Message: fmt.Sprintf("[bridge:rejected] Notion Agent %s returned HTTP %d%s%s", httpErr.Endpoint, httpErr.Status, codeSuffix(httpErr.Code), shapeSuffix(httpErr.Shape))}
	}
	return agentFailure{Kind: "internal", Status: http.StatusBadGateway, ErrType: "api_error", Message: "notion API error: " + err.Error()}
}

// shapeSuffix appends the content-free request shape (counts only) so a rejected
// request can be compared with a passing one without exposing any text.
func shapeSuffix(shape string) string {
	if shape == "" {
		return ""
	}
	return " [request_shape: " + shape + "]"
}

func codeSuffix(code string) string {
	if code == "" {
		return ""
	}
	return " (code=" + code + ")"
}

// writeAgentFailureJSON answers a not-yet-started response with the structured error.
func writeAgentFailureJSON(w http.ResponseWriter, requestID string, f agentFailure) {
	if f.Kind != "internal" {
		w.Header().Set("x-should-retry", fmt.Sprintf("%t", f.ShouldRetry))
		if f.ShouldRetry {
			w.Header().Set("retry-after", "5")
		}
	}
	writeAnthropicError(w, requestID, f.Status, f.Message, f.ErrType)
}

// ---- SSE keepalive ---------------------------------------------------------

// keepaliveResponseWriter wraps an http.ResponseWriter so HandleAnthropicMessages
// can share a single anthropicKeepalive across multi-account failover attempts.
// If attempt 0 exceeds agentKeepaliveDelay (emitting HTTP 200 + message_start +
// ping) and then fails with ErrQuotaExhausted before any content block is sent,
// attempt 1 reuses the same open SSE stream instead of dropping the request.
type keepaliveResponseWriter struct {
	http.ResponseWriter
	ka **anthropicKeepalive
}

func (kw *keepaliveResponseWriter) Flush() {
	if f, ok := kw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (kw *keepaliveResponseWriter) Unwrap() http.ResponseWriter {
	return kw.ResponseWriter
}

func canRetryKeepalive(w http.ResponseWriter) bool {
	_, ok := w.(*keepaliveResponseWriter)
	return ok
}

// anthropicKeepalive owns the start of an Anthropic SSE response. While a remote
// turn is running it emits only message_start and ping events (legal at any
// point of an Anthropic stream); content blocks are never fabricated. The
// goroutine is the only writer until Stop returns, so callers never write
// concurrently with it.
type anthropicKeepalive struct {
	w         http.ResponseWriter
	flusher   http.Flusher
	requestID string
	model     string

	mu          sync.Mutex
	inputTokens int // reported by message_start when the keepalive commits the response
	started     bool
	stopped     bool
	gone        chan struct{} // closed when a keepalive write fails (client disconnected)
	stopCh      chan struct{}
	done        chan struct{}
	goneOnce    sync.Once
}

// newAnthropicKeepalive prepares the response start. The keepalive goroutine is
// only started for the Agent thread transport (the legacy transport answers
// quickly or streams its own text).
func newAnthropicKeepalive(w http.ResponseWriter, requestID, model string) *anthropicKeepalive {
	var slot **anthropicKeepalive
	rawW := w
	if kw, ok := w.(*keepaliveResponseWriter); ok {
		slot = kw.ka
		rawW = kw.ResponseWriter
	}
	if slot != nil && *slot != nil {
		k := *slot
		k.restart()
		return k
	}
	k := &anthropicKeepalive{w: rawW, requestID: requestID, model: model, gone: make(chan struct{}), stopCh: make(chan struct{}), done: make(chan struct{})}
	k.flusher, _ = rawW.(http.Flusher)
	if slot != nil {
		*slot = k
	}
	if k.flusher != nil && AppConfig != nil && AppConfig.Proxy.AgentThread && agentKeepaliveDelay > 0 {
		go k.run(k.stopCh, k.done, false)
	} else {
		close(k.done)
	}
	return k
}

func (k *anthropicKeepalive) restart() {
	k.Stop()
	k.mu.Lock()
	k.stopped = false
	k.stopCh = make(chan struct{})
	k.done = make(chan struct{})
	stopCh := k.stopCh
	done := k.done
	alreadyStarted := k.started
	k.mu.Unlock()
	select {
	case <-k.gone:
		close(done)
		return
	default:
	}
	if k.flusher != nil && AppConfig != nil && AppConfig.Proxy.AgentThread && agentKeepaliveDelay > 0 {
		go k.run(stopCh, done, alreadyStarted)
	} else {
		close(done)
	}
}

func (k *anthropicKeepalive) run(stopCh <-chan struct{}, done chan<- struct{}, alreadyStarted bool) {
	defer close(done)
	if !alreadyStarted {
		timer := time.NewTimer(agentKeepaliveDelay)
		defer timer.Stop()
		select {
		case <-stopCh:
			return
		case <-timer.C:
		}
		if !k.begin(0) { // begin substitutes the SetInputTokens estimate for 0
			return
		}
	}
	interval := agentKeepaliveInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			k.mu.Lock()
			err := k.writeLocked("ping", `{"type":"ping"}`)
			k.mu.Unlock()
			if err != nil {
				k.markGone()
				return
			}
		}
	}
}

// SetInputTokens sets the input token count the keepalive's message_start reports.
func (k *anthropicKeepalive) SetInputTokens(n int) {
	k.mu.Lock()
	k.inputTokens = n
	k.mu.Unlock()
}

func (k *anthropicKeepalive) markGone() {
	k.goneOnce.Do(func() { close(k.gone) })
}

func (k *anthropicKeepalive) writeLocked(event, data string) error {
	if _, err := fmt.Fprintf(k.w, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	k.flusher.Flush()
	return nil
}

// Gone is closed when the client stopped reading the keepalive stream.
func (k *anthropicKeepalive) Gone() <-chan struct{} { return k.gone }

// Started reports whether the SSE response (status 200) is already committed.
func (k *anthropicKeepalive) Started() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.started
}

// Stop ends the keepalive goroutine and waits for it. Idempotent.
func (k *anthropicKeepalive) Stop() {
	k.mu.Lock()
	if !k.stopped {
		k.stopped = true
		close(k.stopCh)
	}
	done := k.done
	k.mu.Unlock()
	<-done
}

// begin commits the SSE response (headers, message_start, ping) exactly once and
// reports whether this call (or an earlier one) started it.
func (k *anthropicKeepalive) begin(inputTokens int) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.started {
		return true
	}
	if k.flusher == nil {
		return false
	}
	k.started = true
	if inputTokens <= 0 {
		inputTokens = k.inputTokens
	}
	h := k.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	k.w.WriteHeader(http.StatusOK)
	sendAnthropicSSE(k.w, k.flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id": k.requestID, "type": "message", "role": "assistant", "content": []interface{}{},
			"model": k.model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]interface{}{"input_tokens": inputTokens, "output_tokens": 0},
		},
	})
	if err := k.writeLocked("ping", `{"type":"ping"}`); err != nil {
		k.markGone()
	}
	return true
}

// fail reports err to the client: as an SSE error event when the stream is
// already committed, otherwise as a structured HTTP error. Agent-transport
// errors carry kind/retry hints; other transports keep the legacy message.
func (k *anthropicKeepalive) fail(err error) {
	f := agentFailureFor(err)
	if k.Started() {
		payload := map[string]interface{}{"type": "error", "error": map[string]interface{}{"type": f.ErrType, "message": f.Message}}
		LogAPIOutputJSON(k.requestID, "anthropic stream error event", payload)
		k.mu.Lock()
		raw, _ := json.Marshal(payload)
		k.writeLocked("error", string(raw))
		k.mu.Unlock()
		return
	}
	writeAgentFailureJSON(k.w, k.requestID, f)
}

// agentFailureFor classifies agent-transport errors; other transports keep the
// pre-existing 502 "notion API error" shape.
func agentFailureFor(err error) agentFailure {
	if errors.Is(err, ErrQuotaExhausted) {
		return quotaExhaustedFailure() // every transport: allowance is gone, not a server fault
	}
	if AppConfig != nil && AppConfig.Proxy.AgentThread {
		return classifyAgentFailure(err)
	}
	return agentFailure{Kind: "internal", Status: http.StatusBadGateway, ErrType: "api_error", Message: "notion API error: " + err.Error()}
}

// writeAnthropicCallError answers a failed inference call on a response that has
// not started (non-stream handlers).
func writeAnthropicCallError(w http.ResponseWriter, requestID string, err error) {
	writeAgentFailureJSON(w, requestID, agentFailureFor(err))
}

// Observe socket cancellation immediately, including before the first SSE ping.
func (k *anthropicKeepalive) WatchClient(gone <-chan struct{}) {
	if gone == nil {
		return
	}
	k.mu.Lock()
	stopCh := k.stopCh
	k.mu.Unlock()
	go func() {
		select {
		case <-gone:
			k.markGone()
		case <-stopCh:
		}
	}()
}
func agentRequestGone(channels []<-chan struct{}) <-chan struct{} {
	if len(channels) > 0 {
		return channels[0]
	}
	return nil
}
