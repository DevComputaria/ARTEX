package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Autumn-27/norma/harness"
)

// runTrace retains the latest tool call so an interrupted run can identify the
// operation that was still in flight.
type runTrace struct {
	startedAt time.Time
	id        string
	name      string
	input     string
	at        time.Time
	pending   bool
}

func (t *runTrace) start(id, name, input string) {
	t.id, t.name, t.input, t.at, t.pending = id, name, input, time.Now(), true
}

func (t *runTrace) done(id string) {
	if id == t.id {
		t.pending = false
	}
}

var reasonHint = map[harness.TerminalReason]string{
	harness.ReasonCompleted:         "The model ended the round normally without a text summary; facts and assets remain available in the tool-call record",
	harness.ReasonMaxTurns:          "The run reached MaxTurns; the SDK completed cleanup and persisted facts and assets, and the intent is marked exhausted",
	harness.ReasonTimeout:           "The run reached MaxDuration; active tools were interrupted, cleanup ran, and recognized facts and assets were persisted",
	harness.ReasonModelError:        "The model or API call failed after retries due to networking, authorization, rate limiting, or a provider error; inspect get_worker_trace before retrying",
	harness.ReasonBlockingLimit:     "The context limit was reached before the request could be sent; narrow the intent or compact its context",
	harness.ReasonPromptTooLong:     "The prompt remained too long after context-compaction retries",
	harness.ReasonImageError:        "The selected model cannot process image content; use a vision-capable model or avoid image-returning tools",
	harness.ReasonStopHookPrevented: "A stop hook prevented the round from ending; check whether the task guard rules are too restrictive",
	harness.ReasonHookStopped:       "A tool or hook stopped execution, for example because the target was out of scope or an operation was disabled",
	harness.ReasonAbortedStreaming:  "The run was cancelled while the model was streaming output",
	harness.ReasonAbortedTools:      "The run was cancelled while a tool was executing",
}

// terminalText renders a terminal event with no final text into a compact summary
// and a Markdown detail block.
func terminalText(ctx context.Context, term *harness.Terminal, tr *runTrace) (string, string) {
	reason := term.Reason
	aborted := reason == harness.ReasonAbortedStreaming || reason == harness.ReasonAbortedTools
	// Prompt may return ctx.Err directly without a terminal event. Preserve the
	// cancellation cause instead of falling back to an empty/unknown terminal reason.
	if reason == "" && ctx.Err() != nil {
		aborted = true
	}

	var sum string
	if aborted {
		_, short, _, ok := AbortReason(ctx)
		if !ok {
			short = "Reason for failure"
		}
		stage := "execution"
		switch reason {
		case harness.ReasonAbortedStreaming:
			stage = "model output"
		case harness.ReasonAbortedTools:
			stage = "tool execution"
		}
		sum = "(Interrupted: " + short + "; stopped during " + stage + progressSuffix(term, tr) + "; unfinished)"
	} else if reason == harness.ReasonMaxTurns || reason == harness.ReasonTimeout {
		sum = "(Execution budget reached: " + string(reason) + "; facts were persisted" + progressSuffix(term, tr) + "; no text summary)"
	} else {
		hint := terminalReasonHint(reason)
		sum = "(No text summary, terminal) " + terminalReasonLabel(reason) + "：" + firstLine(hint, 80) + "）"
	}

	var b strings.Builder
	b.WriteString(sum)
	b.WriteString("\n\n")
	displayReason := terminalReasonLabel(reason)
	fmt.Fprintf(&b, "- **Terminal**: `%s` — %s\n", displayReason, terminalReasonHint(reason))
	if aborted {
		code, _, why, ok := AbortReason(ctx)
		if ok {
			fmt.Fprintf(&b, "- **Interruption reason** (`%s`): %s\n", code, why)
		} else {
			b.WriteString("- **Reason for interruption**: Unable to retrieve; the canceler may not have added a named reason through context.WithCancelCause \n")
		}
	}
	if term.Err != nil {
		fmt.Fprintf(&b, "- **Underlying error**: `%v`\n", term.Err)
	}
	if aborted && strings.TrimSpace(term.Text) != "" {
		b.WriteString("- ** Partial output generated before cancellation** \n \n")
		b.WriteString(term.Text)
		b.WriteString("\n\n")
	}
	if term.Turns > 0 {
		fmt.Fprintf(&b, "- **Completed**: %d model rounds\n", term.Turns)
	}
	if !tr.startedAt.IsZero() {
		fmt.Fprintf(&b, "- **Elapsed time**: %s\n", roundDur(time.Since(tr.startedAt)))
	}
	if u := term.Usage; u.InputTokens+u.OutputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
		fmt.Fprintf(&b, "- **Accumulated tokens**: Input %d / Output %d / Cached read %d / Caching write %d\n",
			u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens)
	}
	if tr.name == "" {
		b.WriteString("- **Tool calls**: the run ended before any tool call\n")
	} else if tr.pending {
		fmt.Fprintf(&b, "- **Tool executing when interrupted**: `%s` (running for %s; **result not returned**)\n\n```json\n%s\n```\n",
			tr.name, roundDur(time.Since(tr.at)), firstLine(tr.input, 300))
	} else {
		fmt.Fprintf(&b, "- **Last tool before interruption**: `%s` (returned normally)\n", tr.name)
	}
	return sum, b.String()
}

func terminalReasonLabel(reason harness.TerminalReason) string {
	if reason == "" {
		return "context_canceled"
	}
	return string(reason)
}

func terminalReasonHint(reason harness.TerminalReason) string {
	if hint := reasonHint[reason]; hint != "" {
		return hint
	}
	if reason == "" {
		return "Running context cancelled, but no Terminal event was generated at the bottom"
	}
	return "Unknown terminal state; harness may have added TerminalReason, please add reasonHint"
}

func progressSuffix(term *harness.Terminal, tr *runTrace) string {
	var parts []string
	if term.Turns > 0 {
		parts = append(parts, fmt.Sprintf("%d rounds", term.Turns))
	}
	if !tr.startedAt.IsZero() {
		parts = append(parts, roundDur(time.Since(tr.startedAt)))
	}
	if len(parts) == 0 {
		return ""
	}
	return ", run " + strings.Join(parts, " / ")
}

func roundDur(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	case d < time.Hour:
		return d.Round(time.Second).String()
	default:
		return d.Round(time.Minute).String()
	}
}
