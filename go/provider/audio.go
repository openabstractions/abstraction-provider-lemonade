package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"

	inference "github.com/openabstractions/abstraction-inference/go"
	wire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/api"
	"github.com/openabstractions/abstraction-provider-kit/go/kokoro"
	"github.com/openabstractions/abstraction-provider-kit/go/whispercpp"
	"github.com/openabstractions/abstraction-provider-lemonade/go/server"
)

// audioServer is what the audio engines need of the owned Lemonade: its
// address and the speech crash bookkeeping (Kokoros review condition 8).
type audioServer interface {
	Base() string
	SpeechFailed(inputDigest string, cause error) error
	SpeechCompleted()
}

// transcriber serves transcription@1 through Lemonade's whisper.cpp backend.
// It builds the request with the whisper.cpp policy package (review
// condition 7) and reads the answer under condition 8; the server transport
// checks the form again before Lemonade sees it.
type transcriber struct {
	server  audioServer
	client  *http.Client
	onError func(error)
}

func (transcriber) Unsupported(req wire.TranscriptionRequest) string {
	switch {
	case req.Model != whispercpp.LargeV3Turbo:
		return "model:" + req.Model
	case req.MediaType != "audio/wav":
		// MP3, MP4, WebM, Ogg and FLAC wait for a reviewed decoder inside
		// the provider; only dr_wav may parse the bytes.
		return "format:" + req.MediaType
	case req.Timestamps == wire.TimestampModeWord || req.Timestamps == wire.TimestampModeSegmentAndWord:
		return "timestamps:" + req.Timestamps.String()
	}
	if _, err := whispercpp.Language(req.Language); err != nil {
		return "language:" + req.Language
	}
	return ""
}

func (t transcriber) Run(ctx context.Context, model string, req wire.TranscriptionRequest, audio []byte, emit func(wire.TranscriptSegment)) wire.TranscriptionReply {
	form, err := whispercpp.NewForm(model, audio, req.Language, "", nil, "verbose_json")
	if err != nil {
		if errors.Is(err, whispercpp.ErrAudio) {
			return wire.TranscriptionReply{Outcome: wire.ReplyOutcomeInvalid, Reason: "audio:wav"}
		}
		return wire.TranscriptionReply{Outcome: wire.ReplyOutcomeInvalid, Reason: "request"}
	}
	body, contentType := form.Body()
	resp, reason := post(ctx, t.client, t.server.Base()+server.TranscriptionRoute, contentType, body)
	if reason != "" {
		report(t.onError, fmt.Errorf("lemonade: transcription: %s", reason))
		return wire.TranscriptionReply{Outcome: replyOutcome(ctx, reason), Reason: reason}
	}
	if outcome, status := upstreamStatus(resp); status != "" {
		return wire.TranscriptionReply{Outcome: outcome, Reason: status}
	}
	answer, err := whispercpp.ReadTranscript(resp, "verbose_json")
	if err != nil {
		report(t.onError, err)
		return wire.TranscriptionReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "upstream:answer"}
	}
	return inference.DecodeTranscript(answer, req.Timestamps, emit)
}

// speaker serves speech@1 through Lemonade's Kokoros backend: English voices,
// NFC text, PCM from koko and the provider's own WAV header (Kokoros review
// conditions 6 and 7). A dropped connection or engine failure is never
// resubmitted (condition 8).
type speaker struct {
	server  audioServer
	client  *http.Client
	onError func(error)
}

func (speaker) Unsupported(req wire.SpeechRequest) string {
	switch {
	case req.Model != kokoro.KokoroV1:
		return "model:" + req.Model
	case req.Format != wire.SpeechFormatWav && req.Format != wire.SpeechFormatPcm:
		return "format:" + req.Format.String()
	}
	if err := kokoro.CheckVoice(req.Voice); err != nil {
		return "voice:" + req.Voice
	}
	if _, err := kokoro.Text(req.Text); err != nil {
		return "text"
	}
	return ""
}

func (s speaker) Run(ctx context.Context, model string, req wire.SpeechRequest) inference.SpeechEngineReply {
	if _, err := kokoro.NewSynthesis(model, req.Text, req.Voice, 1); err != nil {
		return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeInvalid, Reason: "request"}
	}
	// The server transport rebuilds this OpenAI-shaped body with the
	// policy package (kokoro.ParseSynthesis) before Lemonade sees it.
	body, err := json.Marshal(map[string]any{"model": model, "input": req.Text, "voice": req.Voice, "response_format": "pcm", "speed": 1})
	if err != nil {
		return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeInvalid, Reason: "request"}
	}
	digest := kokoro.InputDigest(req.Text)
	crashed := func(cause error) inference.SpeechEngineReply {
		report(s.onError, s.server.SpeechFailed(digest, cause))
		return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "engine:crashed"}
	}
	resp, reason := post(ctx, s.client, s.server.Base()+server.SpeechRoute, "application/json", body)
	if reason == "upstream:unreachable" && ctx.Err() == nil {
		return crashed(errors.New("the request failed in flight"))
	}
	if reason != "" {
		report(s.onError, fmt.Errorf("lemonade: speech of %s: %s", digest, reason))
		return inference.SpeechEngineReply{Outcome: replyOutcome(ctx, reason), Reason: reason}
	}
	if resp.StatusCode >= 500 {
		//unchecked: best-effort drain; the status decides the outcome
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		return crashed(fmt.Errorf("status %d", resp.StatusCode))
	}
	if outcome, status := upstreamStatus(resp); status != "" {
		return inference.SpeechEngineReply{Outcome: outcome, Reason: status}
	}
	pcm, err := kokoro.ReadPCM(resp)
	if err != nil {
		var netErr net.Error
		if ctx.Err() == nil && (errors.As(err, &netErr) || errors.Is(err, io.ErrUnexpectedEOF)) {
			return crashed(err)
		}
		report(s.onError, err)
		return inference.SpeechEngineReply{Outcome: replyOutcome(ctx, "upstream:answer"), Reason: "upstream:answer"}
	}
	s.server.SpeechCompleted()
	if req.Format == wire.SpeechFormatPcm {
		return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeCompleted, Audio: pcm}
	}
	wav, err := kokoro.WAV(pcm)
	if err != nil {
		return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeUnavailable, Reason: "upstream:answer"}
	}
	return inference.SpeechEngineReply{Outcome: wire.ReplyOutcomeCompleted, Audio: wav}
}

// post sends one request once. A refusal of the server transport names its
// condition (engine:audio:<condition>, engine:unavailable, engine:route);
// a failure on the connection to the engine is upstream:unreachable. The
// provider never retries.
func post(ctx context.Context, client *http.Client, url, contentType string, body []byte) (*http.Response, string) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, "upstream:address"
	}
	httpReq.Header.Set("Content-Type", contentType)
	resp, err := client.Do(httpReq)
	if err != nil {
		var refusal *server.AudioRefusal
		switch {
		case errors.As(err, &refusal):
			return nil, server.RefusalReason(err)
		case errors.Is(err, server.ErrForbiddenRoute):
			return nil, "engine:route"
		case errors.Is(err, server.ErrUnavailable):
			return nil, "engine:unavailable"
		}
		return nil, "upstream:unreachable"
	}
	return resp, ""
}

// upstreamStatus reads a non-200 answer: 429 and 5xx are unavailable, any
// other status refused, each with reason upstream:<status>. It closes such a
// body; a 200 answer is left to the caller.
func upstreamStatus(resp *http.Response) (wire.ReplyOutcome, string) {
	if resp.StatusCode == http.StatusOK {
		return 0, ""
	}
	//unchecked: best-effort drain; the status decides the outcome
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
	outcome := wire.ReplyOutcomeRefused
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		outcome = wire.ReplyOutcomeUnavailable
	}
	return outcome, fmt.Sprintf("upstream:%d", resp.StatusCode)
}

func replyOutcome(ctx context.Context, _ string) wire.ReplyOutcome {
	if ctx.Err() != nil {
		return wire.ReplyOutcomeCancelled
	}
	return wire.ReplyOutcomeUnavailable
}
