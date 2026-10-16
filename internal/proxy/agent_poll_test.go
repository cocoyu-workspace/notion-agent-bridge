package proxy

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func setPollHedge(t *testing.T, d time.Duration) {
	t.Helper()
	old := agentPollHedgeDelay
	agentPollHedgeDelay = d
	t.Cleanup(func() { agentPollHedgeDelay = old })
}

// A stalled transcript call must not hold the reply: a parallel identical
// poll is started and the first answer wins; the stalled one is cancelled.
func TestHedgedPollAnswersDespiteAStalledCall(t *testing.T) {
	setPollHedge(t, 30*time.Millisecond)
	var calls atomic.Int32
	stalledCancelled := make(chan struct{})
	start := time.Now()
	v, err := hedgedPoll(context.Background(), func(ctx context.Context) (int, error) {
		if calls.Add(1) == 1 {
			<-ctx.Done() // Notion stalls this call
			close(stalledCancelled)
			return 0, ctx.Err()
		}
		return 7, nil
	})
	if err != nil || v != 7 || calls.Load() != 2 {
		t.Fatalf("v=%d err=%v calls=%d, want 7 nil 2", v, err, calls.Load())
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	select {
	case <-stalledCancelled:
	case <-time.After(time.Second):
		t.Fatal("stalled poll was not cancelled")
	}
}

// A slow but working poll is never abandoned: with every attempt slow, the
// first to finish is returned, and no more than agentPollMaxInFlight run.
func TestHedgedPollKeepsSlowPollsAndBoundsParallelism(t *testing.T) {
	setPollHedge(t, 10*time.Millisecond)
	var calls atomic.Int32
	v, err := hedgedPoll(context.Background(), func(ctx context.Context) (int, error) {
		n := calls.Add(1)
		select {
		case <-time.After(150 * time.Millisecond):
			return int(n), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	})
	if err != nil || v != 1 {
		t.Fatalf("v=%d err=%v, want the first (slow) attempt's answer", v, err)
	}
	if n := calls.Load(); n != int32(agentPollMaxInFlight) {
		t.Fatalf("calls=%d, want %d", n, agentPollMaxInFlight)
	}
}

func TestHedgedPollFastPathAndErrors(t *testing.T) {
	setPollHedge(t, time.Second)
	var calls atomic.Int32
	boom := errors.New("boom")
	if _, err := hedgedPoll(context.Background(), func(context.Context) (int, error) { calls.Add(1); return 0, boom }); err != boom || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d, want boom once (no extra request on the fast path)", err, calls.Load())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hedgedPoll(ctx, func(c context.Context) (int, error) { return 0, c.Err() }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v, want canceled", err)
	}
}

func TestHedgedPollBoundsHungAttemptWhenParentDeadlineIsLong(t *testing.T) {
	setPollHedge(t, 20*time.Millisecond)
	oldAttemptTimeout := agentPollAttemptTimeout
	agentPollAttemptTimeout = 80 * time.Millisecond
	t.Cleanup(func() { agentPollAttemptTimeout = oldAttemptTimeout })

	parentCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()
	_, err := hedgedPoll(parentCtx, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context.DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("hedgedPoll waited %v instead of bounding individual poll attempts", elapsed)
	}
}

func TestRebuildChromeTransportSwapsActiveH2TransportForExistingClients(t *testing.T) {
	client := getChromeHTTPClient(5 * time.Second)
	initial := currentChromeH2Transport()
	if initial == nil {
		t.Fatal("expected initialized Chrome H2 transport")
	}
	if initial.ReadIdleTimeout != chromeH2ReadIdleTimeout || initial.PingTimeout != chromeH2PingTimeout || initial.WriteByteTimeout != chromeH2WriteByteTimeout {
		t.Fatalf("unexpected H2 timeouts: readIdle=%v ping=%v writeByte=%v", initial.ReadIdleTimeout, initial.PingTimeout, initial.WriteByteTimeout)
	}
	RebuildChromeTransport()
	rebuilt := currentChromeH2Transport()
	if rebuilt == nil || rebuilt == initial {
		t.Fatal("RebuildChromeTransport did not replace the active http2.Transport instance")
	}
	if client.Transport != getChromeRoundTripper() {
		t.Fatal("existing http.Client does not use dynamic Chrome RoundTripper")
	}
}

func TestHedgedPollRebuildsChromeTransportOnHedge(t *testing.T) {
	setPollHedge(t, 25*time.Millisecond)
	initial := currentChromeH2Transport()
	var n atomic.Int32
	v, err := hedgedPoll(context.Background(), func(ctx context.Context) (string, error) {
		if n.Add(1) == 1 {
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "ok", nil
	})
	if err != nil || v != "ok" {
		t.Fatalf("v=%q err=%v", v, err)
	}
	if currentChromeH2Transport() == initial {
		t.Fatal("expected hedgedPoll to rotate Chrome H2 transport before launching hedged attempt")
	}
}

func TestDefaultPollHedgeDelayAndInferenceTimeoutAvoidLargeTranscriptChurn(t *testing.T) {
	if agentPollHedgeDelay < 5*time.Second {
		t.Fatalf("agentPollHedgeDelay=%v, want >= 5s so 200KB+ transcripts (2-3s normal RTT) do not trigger false-positive hedges and TLS rebuilds", agentPollHedgeDelay)
	}
	cfg := DefaultConfig()
	if cfg.Timeouts.InferenceTimeout < 600 {
		t.Fatalf("DefaultConfig().Timeouts.InferenceTimeout=%d, want >= 600s for max-effort Agent reasoning turns", cfg.Timeouts.InferenceTimeout)
	}
}


