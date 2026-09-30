package helps

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	"github.com/tidwall/gjson"
	"github.com/tiktoken-go/tokenizer"
)

// streamedOutputCounter estimates output tokens from streamed deltas so an
// attempt that ends before its terminal usage event can still be accounted.
type streamedOutputCounter struct {
	codecOnce sync.Once
	codec     tokenizer.Codec
	tokens    atomic.Int64
}

func isCodexOutputDeltaEvent(eventType string) bool {
	switch eventType {
	case "response.output_text.delta",
		"response.refusal.delta",
		"response.reasoning_text.delta",
		"response.reasoning_summary_text.delta",
		"response.function_call_arguments.delta",
		"response.custom_tool_call_input.delta":
		return true
	}
	return false
}

// ObserveCodexOutputDelta adds the tokens of a streamed Codex output delta to
// the running estimate used by PublishInterrupted.
func (r *UsageReporter) ObserveCodexOutputDelta(payload []byte) {
	if r == nil || len(payload) == 0 {
		return
	}
	if !isCodexOutputDeltaEvent(gjson.GetBytes(payload, "type").String()) {
		return
	}
	delta := gjson.GetBytes(payload, "delta").String()
	if delta == "" {
		return
	}
	counter := &r.streamedOutput
	counter.codecOnce.Do(func() {
		codec, errCodec := TokenizerForModel(r.model)
		if errCodec == nil {
			counter.codec = codec
		}
	})
	if counter.codec == nil {
		// Fall back to the common ~4 bytes per token heuristic.
		counter.tokens.Add(int64((len(delta) + 3) / 4))
		return
	}
	count, errCount := counter.codec.Count(delta)
	if errCount != nil {
		counter.tokens.Add(int64((len(delta) + 3) / 4))
		return
	}
	counter.tokens.Add(int64(count))
}

// PublishInterrupted records an attempt that ended without a terminal usage
// event, typically because the client cancelled the stream. Output tokens are a
// lower-bound estimate from streamed deltas (hidden reasoning is not streamed).
// Input tokens are left at zero because the cached share is unknown and
// estimating them as uncached would overcharge the user. It is a no-op when
// the attempt was already published.
func (r *UsageReporter) PublishInterrupted(ctx context.Context) {
	if r == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.Publish(ctx, usage.Detail{OutputTokens: r.streamedOutput.tokens.Load()})
}
