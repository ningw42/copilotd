package sse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestPumpNilAndIdentityTransformersPreserveReaderCorpusFrameForFrame(t *testing.T) {
	const (
		lfFrame       = "event: message_start\ndata: {\"type\":\"ignored\"}\n\n"
		commentFrame  = ": upstream keepalive\n\n"
		unknownFrame  = ": vendor metadata\r\nevent: vendor.future_event\r\ndata: {\"unknown\":true,\r\ndata: \"kept\":\"verbatim\"}\r\n\r\n"
		terminalFrame = "data: {\r\ndata: \"type\": \"response.completed\",\r\ndata: \"unknown\": true\r\ndata: }\r\n\r\n"
	)
	wantOps := []string{
		"write:" + lfFrame, "flush",
		"write:" + commentFrame, "flush",
		"write:" + unknownFrame, "flush",
		"write:" + terminalFrame, "flush",
	}

	tests := []struct {
		name        string
		transformer FrameTransformer
	}{
		{name: "nil"},
		{name: "identity", transformer: identityFrameTransformer{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := &operationResponseWriter{header: make(http.Header)}
			ctx, cancel := context.WithCancel(context.Background())

			result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(lfFrame+commentFrame+unknownFrame+terminalFrame)), dst, Policy{
				Terminal: func(eventType string) bool { return eventType == "response.completed" },
				RenderError: func(http.ResponseWriter, Outcome) error {
					t.Fatal("complete corpus unexpectedly synthesized an error")
					return nil
				},
				WriteTimeout: time.Second,
				Clock:        RealClock{},
			}, tt.transformer)

			if result.Outcome != OutcomeClean {
				t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
			}
			if result.Frames != 4 {
				t.Errorf("Frames = %d, want 4 written frames", result.Frames)
			}
			if len(dst.ops) != len(wantOps) {
				t.Fatalf("operations = %#v, want %#v", dst.ops, wantOps)
			}
			for i := range wantOps {
				if dst.ops[i] != wantOps[i] {
					t.Errorf("operation %d = %q, want %q", i, dst.ops[i], wantOps[i])
				}
			}
		})
	}
}

func TestPumpHeldFrameDoesNotResetClientIdleKeepalive(t *testing.T) {
	const held = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\"}\n\n"
	upstream, send := io.Pipe()
	dst := &operationResponseWriter{header: make(http.Header), wrote: make(chan struct{}, 1)}
	clock := newManualClock()
	transformed := make(chan struct{})
	transformer := frameTransformerFuncs{
		transform: func(context.Context, Frame) ([]Frame, error) {
			close(transformed)
			return nil, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- Pump(ctx, cancel, upstream, dst, Policy{
			Terminal:          func(string) bool { return false },
			RenderError:       func(http.ResponseWriter, Outcome) error { return nil },
			WriteTimeout:      time.Minute,
			IdleTimeout:       time.Minute,
			KeepaliveInterval: 10 * time.Second,
			Clock:             clock,
		}, transformer)
	}()
	<-clock.timerCreated // stall
	<-clock.timerCreated // client idle
	clock.Advance(6 * time.Second)
	if _, err := io.WriteString(send, held); err != nil {
		t.Fatalf("write held upstream frame: %v", err)
	}
	<-transformed

	clock.Advance(4 * time.Second)
	select {
	case <-dst.wrote:
	case <-time.After(time.Second):
		t.Fatal("held frame reset client-idle timing; keepalive was not written at the original deadline")
	}
	cancel()
	result := <-done
	_ = send.Close()

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
	if len(dst.ops) != 2 || dst.ops[0] != "write::\n\n" || dst.ops[1] != "flush" {
		t.Errorf("operations = %#v, want one keepalive Write/Flush and no held-frame output", dst.ops)
	}
}

func TestPumpFinalizesHeldCompleteStreamAtEOF(t *testing.T) {
	const first = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var held []Frame
	finalized := false
	transformer := frameTransformerFuncs{
		transform: func(_ context.Context, frame Frame) ([]Frame, error) {
			held = append(held, frame)
			return nil, nil
		},
		finalize: func(context.Context) ([]Frame, error) {
			finalized = true
			return held, nil
		},
	}
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(first+terminal)), dst, Policy{
		Terminal: func(eventType string) bool { return eventType == "message_stop" },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("held terminal at EOF unexpectedly synthesized an error")
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, transformer)

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if !finalized {
		t.Error("Finalize was not called for a pre-terminal EOF")
	}
	if result.Frames != 2 {
		t.Errorf("Frames = %d, want 2 finalized client-facing frames", result.Frames)
	}
	wantOps := []string{"write:" + first, "flush", "write:" + terminal, "flush"}
	if strings.Join(dst.ops, "|") != strings.Join(wantOps, "|") {
		t.Errorf("operations = %#v, want %#v", dst.ops, wantOps)
	}
}

func TestPumpRecordsTransformErrorSuppressedAfterWrittenTerminal(t *testing.T) {
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	const afterTerminal = "event: vendor.trailing\ndata: SECRET-FRAME-CONTENT\n\n"
	finalized := false
	transformer := frameTransformerFuncs{
		transform: func(_ context.Context, frame Frame) ([]Frame, error) {
			if frame.Type == "message_stop" {
				return []Frame{frame}, nil
			}
			return nil, errors.New("SECRET-TRANSFORM-ERROR")
		},
		finalize: func(context.Context) ([]Frame, error) {
			finalized = true
			return []Frame{{Type: "held", Raw: []byte("SECRET-HELD-FRAME")}}, nil
		},
	}
	var logs bytes.Buffer
	counter := NewSuppressedShimErrorCounter()
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())

	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(terminal+afterTerminal)), dst, Policy{
		Terminal: func(eventType string) bool { return eventType == "message_stop" },
		RenderError: func(http.ResponseWriter, Outcome) error {
			t.Error("suppressed transform error rendered a terminal")
			return nil
		},
		WriteTimeout:         time.Second,
		Clock:                RealClock{},
		Logger:               slog.New(slog.NewTextHandler(&logs, nil)),
		SuppressedShimErrors: counter,
	}, transformer)

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	if finalized {
		t.Error("Finalize ran after a Transform error")
	}
	if got := strings.Join(dst.ops, "|"); got != strings.Join([]string{"write:" + terminal, "flush"}, "|") {
		t.Errorf("operations = %#v, want only the written terminal", dst.ops)
	}
	if counter.Count() != 1 {
		t.Errorf("suppressed shim error count = %d, want 1", counter.Count())
	}
	logText := logs.String()
	if !strings.Contains(logText, "suppressed post-terminal shim error") || !strings.Contains(logText, "stage=transform") {
		t.Errorf("warning = %q, want suppression message and transform stage", logText)
	}
	for _, secret := range []string{"SECRET-TRANSFORM-ERROR", "SECRET-FRAME-CONTENT", "SECRET-HELD-FRAME"} {
		if strings.Contains(logText, secret) {
			t.Errorf("warning leaked %q: %s", secret, logText)
		}
	}
}

func TestPumpFinalizesHeldCompleteStreamOnStall(t *testing.T) {
	const first = "event: message_start\ndata: {\"type\":\"message_start\"}\n\n"
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	body := newBlockingAfterFrameBody(first + terminal)
	clock := newManualClock()
	transformed := make(chan struct{}, 2)
	var held []Frame
	transformer := frameTransformerFuncs{
		transform: func(_ context.Context, frame Frame) ([]Frame, error) {
			held = append(held, frame)
			transformed <- struct{}{}
			return nil, nil
		},
		finalize: func(context.Context) ([]Frame, error) { return held, nil },
	}
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal: func(eventType string) bool { return eventType == "message_stop" },
			RenderError: func(http.ResponseWriter, Outcome) error {
				t.Error("held terminal at stall unexpectedly synthesized an error")
				return nil
			},
			WriteTimeout: time.Minute,
			IdleTimeout:  10 * time.Second,
			Clock:        clock,
		}, transformer)
	}()
	<-clock.timerCreated
	<-transformed
	<-clock.timerReset
	<-transformed
	<-clock.timerReset
	clock.Advance(10 * time.Second)
	result := <-done

	if result.Outcome != OutcomeClean {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClean)
	}
	wantOps := []string{"write:" + first, "flush", "write:" + terminal, "flush"}
	if strings.Join(dst.ops, "|") != strings.Join(wantOps, "|") {
		t.Errorf("operations = %#v, want finalized held stream %#v", dst.ops, wantOps)
	}
}

func TestPumpStopsStallStopwatchBeforeSlowTransform(t *testing.T) {
	const frame = "event: response.output_text.delta\ndata: {}\n\n"
	body := newBlockingAfterFrameBody(frame)
	clock := newManualClock()
	entered := make(chan struct{})
	release := make(chan struct{})
	transformer := frameTransformerFuncs{
		transform: func(_ context.Context, frame Frame) ([]Frame, error) {
			close(entered)
			<-release
			return []Frame{frame}, nil
		},
	}
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- Pump(ctx, cancel, body, dst, Policy{
			Terminal:     func(string) bool { return false },
			RenderError:  func(http.ResponseWriter, Outcome) error { return nil },
			WriteTimeout: time.Minute,
			IdleTimeout:  10 * time.Second,
			Clock:        clock,
		}, transformer)
	}()
	<-clock.timerCreated
	<-entered
	clock.Advance(time.Minute)
	select {
	case <-clock.timerFired:
		t.Error("stall stopwatch fired while Transform was running")
	default:
	}
	close(release)
	<-clock.timerReset
	cancel()
	result := <-done

	if result.Outcome != OutcomeClientCancel {
		t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
	}
}

func TestPumpDroppedTerminalDoesNotCountAsWrittenTerminal(t *testing.T) {
	const terminal = "event: message_stop\ndata: {}\n\n"
	rendered := 0
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(terminal)), dst, Policy{
		Terminal: func(eventType string) bool { return eventType == "message_stop" },
		RenderError: func(_ http.ResponseWriter, outcome Outcome) error {
			rendered++
			if outcome != OutcomeSynthesized {
				t.Errorf("render outcome = %q, want %q", outcome, OutcomeSynthesized)
			}
			return nil
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, frameTransformerFuncs{transform: func(context.Context, Frame) ([]Frame, error) { return nil, nil }})

	if result.Outcome != OutcomeSynthesized || rendered != 1 {
		t.Errorf("Result = %#v, rendered = %d; want one synthesized terminal", result, rendered)
	}
	if len(dst.ops) != 0 || result.Frames != 0 {
		t.Errorf("dropped terminal produced operations %#v or %d frames", dst.ops, result.Frames)
	}
}

func TestPumpTransformErrorBeforeTerminalSkipsFinalizeAndRendersShimError(t *testing.T) {
	const first = "event: response.output_text.delta\ndata: {\"delta\":\"held\"}\n\n"
	const second = "event: response.output_text.delta\ndata: {\"delta\":\"fails\"}\n\n"
	transformCalls := 0
	finalized := false
	transformer := frameTransformerFuncs{
		transform: func(context.Context, Frame) ([]Frame, error) {
			transformCalls++
			if transformCalls == 1 {
				return nil, nil
			}
			return nil, errors.New("transform failed")
		},
		finalize: func(context.Context) ([]Frame, error) {
			finalized = true
			return []Frame{{Type: "held", Raw: []byte("held must be discarded")}}, nil
		},
	}
	rendered := 0
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(first+second)), dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(w http.ResponseWriter, outcome Outcome) error {
			rendered++
			if outcome != OutcomeShimError {
				t.Errorf("render outcome = %q, want %q", outcome, OutcomeShimError)
			}
			if _, err := io.WriteString(w, "native shim terminal"); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, transformer)

	if result.Outcome != OutcomeShimError || rendered != 1 {
		t.Errorf("Result = %#v, rendered = %d; want exactly one shim_error", result, rendered)
	}
	if finalized {
		t.Error("Finalize ran after a Transform error")
	}
	if got := strings.Join(dst.ops, "|"); got != "write:native shim terminal|flush" {
		t.Errorf("operations = %#v, want only the synthesized shim terminal", dst.ops)
	}
}

func TestPumpRecordsFinalizeErrorSuppressedByFinalizedTerminal(t *testing.T) {
	const input = "event: response.output_text.delta\ndata: SECRET-INPUT-FRAME\n\n"
	const terminal = "event: response.completed\ndata: {}\n\n"
	var logs bytes.Buffer
	counter := NewSuppressedShimErrorCounter()
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(input)), dst, Policy{
		Terminal:             func(eventType string) bool { return eventType == "response.completed" },
		RenderError:          func(http.ResponseWriter, Outcome) error { t.Error("finalized terminal was doubled"); return nil },
		WriteTimeout:         time.Second,
		Clock:                RealClock{},
		Logger:               slog.New(slog.NewTextHandler(&logs, nil)),
		SuppressedShimErrors: counter,
	}, frameTransformerFuncs{
		transform: func(context.Context, Frame) ([]Frame, error) { return nil, nil },
		finalize: func(context.Context) ([]Frame, error) {
			return []Frame{{Type: "response.completed", Raw: []byte(terminal)}}, errors.New("SECRET-FINALIZE-ERROR")
		},
	})

	if result.Outcome != OutcomeClean || result.Frames != 1 {
		t.Errorf("Result = %#v, want one finalized terminal and clean outcome", result)
	}
	if counter.Count() != 1 {
		t.Errorf("suppressed shim error count = %d, want 1", counter.Count())
	}
	logText := logs.String()
	if !strings.Contains(logText, "suppressed post-terminal shim error") || !strings.Contains(logText, "stage=finalize") {
		t.Errorf("warning = %q, want suppression message and finalize stage", logText)
	}
	for _, secret := range []string{"SECRET-INPUT-FRAME", "SECRET-FINALIZE-ERROR"} {
		if strings.Contains(logText, secret) {
			t.Errorf("warning leaked %q: %s", secret, logText)
		}
	}
}

func TestPumpFinalizeErrorWithoutTerminalRendersShimErrorAfterBatch(t *testing.T) {
	const input = "event: response.output_text.delta\ndata: {}\n\n"
	const finalized = "event: vendor.held\ndata: {}\n\n"
	dst := &operationResponseWriter{header: make(http.Header)}
	ctx, cancel := context.WithCancel(context.Background())
	result := Pump(ctx, cancel, io.NopCloser(strings.NewReader(input)), dst, Policy{
		Terminal: func(string) bool { return false },
		RenderError: func(w http.ResponseWriter, outcome Outcome) error {
			if outcome != OutcomeShimError {
				t.Errorf("render outcome = %q, want %q", outcome, OutcomeShimError)
			}
			if _, err := io.WriteString(w, "native shim terminal"); err != nil {
				return err
			}
			return http.NewResponseController(w).Flush()
		},
		WriteTimeout: time.Second,
		Clock:        RealClock{},
	}, frameTransformerFuncs{
		transform: func(context.Context, Frame) ([]Frame, error) { return nil, nil },
		finalize: func(context.Context) ([]Frame, error) {
			return []Frame{{Type: "vendor.held", Raw: []byte(finalized)}}, errors.New("finalize failed")
		},
	})

	if result.Outcome != OutcomeShimError || result.Frames != 1 {
		t.Errorf("Result = %#v, want finalized non-terminal then shim_error", result)
	}
	wantOps := []string{"write:" + finalized, "flush", "write:native shim terminal", "flush"}
	if strings.Join(dst.ops, "|") != strings.Join(wantOps, "|") {
		t.Errorf("operations = %#v, want %#v", dst.ops, wantOps)
	}
}

func TestPumpClientDisconnectSkipsFinalizeRegardlessOfWrittenTerminal(t *testing.T) {
	const terminal = "event: message_stop\ndata: {}\n\n"
	const held = "event: vendor.trailing\ndata: {}\n\n"
	tests := []struct {
		name     string
		upstream string
		wantOps  []string
	}{
		{name: "before terminal", upstream: held},
		{name: "after written terminal", upstream: terminal + held, wantOps: []string{"write:" + terminal, "flush"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := newBlockingAfterFrameBody(tt.upstream)
			transformed := make(chan struct{}, 2)
			finalized := false
			transformer := frameTransformerFuncs{
				transform: func(_ context.Context, frame Frame) ([]Frame, error) {
					transformed <- struct{}{}
					if frame.Type == "message_stop" {
						return []Frame{frame}, nil
					}
					return nil, nil
				},
				finalize: func(context.Context) ([]Frame, error) {
					finalized = true
					return nil, nil
				},
			}
			dst := &operationResponseWriter{header: make(http.Header)}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan Result, 1)
			go func() {
				done <- Pump(ctx, cancel, body, dst, Policy{
					Terminal:     func(eventType string) bool { return eventType == "message_stop" },
					RenderError:  func(http.ResponseWriter, Outcome) error { t.Error("disconnect synthesized an error"); return nil },
					WriteTimeout: time.Second,
					Clock:        RealClock{},
				}, transformer)
			}()
			for range strings.Count(tt.upstream, "\n\n") {
				<-transformed
			}
			cancel()
			result := <-done

			if result.Outcome != OutcomeClientCancel {
				t.Errorf("Outcome = %q, want %q", result.Outcome, OutcomeClientCancel)
			}
			if finalized {
				t.Error("Finalize ran after client disconnect")
			}
			if strings.Join(dst.ops, "|") != strings.Join(tt.wantOps, "|") {
				t.Errorf("operations = %#v, want %#v", dst.ops, tt.wantOps)
			}
		})
	}
}

type identityFrameTransformer struct{}

func (identityFrameTransformer) Transform(_ context.Context, frame Frame) ([]Frame, error) {
	return []Frame{frame}, nil
}

func (identityFrameTransformer) Finalize(context.Context) ([]Frame, error) { return nil, nil }

type frameTransformerFuncs struct {
	transform func(context.Context, Frame) ([]Frame, error)
	finalize  func(context.Context) ([]Frame, error)
}

func (f frameTransformerFuncs) Transform(ctx context.Context, frame Frame) ([]Frame, error) {
	return f.transform(ctx, frame)
}

func (f frameTransformerFuncs) Finalize(ctx context.Context) ([]Frame, error) {
	if f.finalize == nil {
		return nil, nil
	}
	return f.finalize(ctx)
}

type operationResponseWriter struct {
	header http.Header
	ops    []string
	wrote  chan struct{}
}

func (w *operationResponseWriter) Header() http.Header              { return w.header }
func (w *operationResponseWriter) WriteHeader(int)                  {}
func (w *operationResponseWriter) SetWriteDeadline(time.Time) error { return nil }

func (w *operationResponseWriter) Write(p []byte) (int, error) {
	w.ops = append(w.ops, "write:"+string(p))
	if w.wrote != nil {
		select {
		case w.wrote <- struct{}{}:
		default:
		}
	}
	return len(p), nil
}

func (w *operationResponseWriter) Flush() { w.ops = append(w.ops, "flush") }
