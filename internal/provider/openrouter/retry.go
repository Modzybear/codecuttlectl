package openrouter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRateLimitRetries = 2
const maxRateLimitWait = 2 * time.Minute
const maxMalformedRetries = 3

// malformedResponseError represents an upstream provider generation failure
// (e.g. Gemini MALFORMED_RESPONSE / MALFORMED_FUNCTION_CALL) that OpenRouter
// may normalize to finish_reason:"stop" or "error". Bounded retries are
// adapter-owned so outer conversation loops do not multiply retries.
type malformedResponseError struct {
	finishReason string
	nativeReason string
	partial      bool
	reason       string
}

func (e *malformedResponseError) Error() string {
	target := e.nativeReason
	var message string
	if target != "" {
		message = fmt.Sprintf("openrouter: upstream malformed response (%s)", target)
	} else {
		message = fmt.Sprintf("openrouter: upstream generation error (finish_reason=%s)", e.finishReason)
	}
	if e.partial {
		return message + "; partial response received, automatic retry suppressed"
	}
	if e.reason != "" {
		return message + "; " + e.reason
	}
	return message
}

func (*malformedResponseError) RetryHandled() bool { return true }

// isMalformedNativeFinishReason checks for upstream generation failure reasons.
func isMalformedNativeFinishReason(native string) bool {
	upper := strings.ToUpper(strings.TrimSpace(native))
	switch upper {
	case "MALFORMED_RESPONSE", "MALFORMED_FUNCTION_CALL", "UNEXPECTED_TOOL_CALL":
		return true
	default:
		return false
	}
}

// classifyAbnormalFinish inspects finish_reason and native_finish_reason.
// Returns (err, true) when the stream/completion terminated abnormally.
// Only explicit malformed native reasons (and bare finish_reason:"error" with
// no native reason) are retryable via malformedResponseError; explicit
// policy/safety or other non-malformed native reasons with finish_reason:"error"
// surface immediately as non-retryable upstream errors without replaying.
func classifyAbnormalFinish(finishReason, nativeFinishReason string, partial bool) (error, bool) {
	native := strings.TrimSpace(nativeFinishReason)
	if isMalformedNativeFinishReason(native) {
		return &malformedResponseError{
			finishReason: finishReason,
			nativeReason: native,
			partial:      partial,
		}, true
	}
	if strings.EqualFold(finishReason, "error") {
		if native == "" {
			return &malformedResponseError{
				finishReason: finishReason,
				partial:      partial,
			}, true
		}
		return fmt.Errorf("openrouter: upstream generation failed (%s)", native), true
	}
	return nil, false
}

func malformedRetryDelay(err error, retries int) (time.Duration, bool) {
	var mErr *malformedResponseError
	if !errors.As(err, &mErr) || mErr.partial {
		return 0, false
	}
	if retries >= maxMalformedRetries {
		mErr.reason = fmt.Sprintf("retry limit reached (%d attempts); try again", retries+1)
		return 0, false
	}
	delay := time.Duration(1<<uint(retries)) * time.Second
	return delay, true
}

// rateLimitError is handled by this adapter, not an outer conversation retry
// loop. Retrying after any generated content requires explicit user action.
type rateLimitError struct {
	retryAfter time.Duration
	partial    bool
	reason     string
}

func (e *rateLimitError) Error() string {
	message := "openrouter: rate limited (429)"
	if e.partial {
		return message + "; partial response received, automatic retry suppressed"
	}
	if e.reason != "" {
		return message + "; " + e.reason
	}
	return message
}
func (*rateLimitError) Throttled() bool    { return true }
func (*rateLimitError) RetryHandled() bool { return true }

// Unknown/invalid headers fall back to exponential backoff. Never overflow a
// duration or shorten a valid server wait: oversized waits require manual retry.
func parseRetryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		if seconds > int64(maxRateLimitWait/time.Second) {
			return maxRateLimitWait + time.Second
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

type responseBody struct {
	io.ReadCloser
	retryAfter time.Duration
}

func rateLimitDelay(err error, retries int) (time.Duration, bool) {
	var limit *rateLimitError
	if !errors.As(err, &limit) || limit.partial {
		return 0, false
	}
	if retries >= maxRateLimitRetries {
		limit.reason = fmt.Sprintf("retry limit reached (%d attempts); try again later", retries+1)
		return 0, false
	}
	if limit.retryAfter > maxRateLimitWait {
		limit.reason = "server requested a wait longer than 2 minutes; try again later"
		return 0, false
	}
	delay := 30 * time.Second << uint(retries)
	if limit.retryAfter > 0 {
		delay = limit.retryAfter
	}
	return delay, true
}

func waitForRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
