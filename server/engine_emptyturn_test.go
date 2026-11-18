package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// For identification and rerun of idle loops (thinking only, no text, no tools), see
// steerHooks.Stop.

func assistantThinking(text string) llm.Message {
	return llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: text, Signature: "sig"},
	}}
}

func TestIsThinkingOnlyTurn(t *testing.T) {
	toolUse := llm.Message{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
		{Type: llm.BlockThinking, Thinking: "Let's clean the port first."},
		{Type: llm.BlockToolUse, ID: "t1", Name: "run_nuclei"},
	}}
	cases := []struct {
		name string
		msgs []llm.Message
		want bool
	}{
		{"Just think.", []llm.Message{llm.UserText("Action."), assistantThinking("Think about it.")}, true},
		{"Thinking + Tools", []llm.Message{llm.UserText("Action."), toolUse}, false},
		{"Thinking + Body", []llm.Message{assistantThinking("Think about it."), {
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("Conclusions")},
		}}, false},
		{"Text only blank characters", []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{{Type: llm.BlockThinking, Thinking: "x"}, llm.TextBlock("  \n ")},
		}}, true},
		{"Completely empty assistant round", []llm.Message{{Role: llm.RoleAssistant}}, true},
		// The tool results in a user role, which determines that it must go back to the assistant
		// before it, rather than just near the error.
		{"The last one is the result of the tool.", []llm.Message{toolUse, {
			Role:    llm.RoleUser,
			Content: []llm.ContentBlock{{Type: llm.BlockToolResult, ToolUseID: "t1"}},
		}}, false},
		{"No message assistant", []llm.Message{llm.UserText("Action.")}, false},
		{"Empty History", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isThinkingOnlyTurn(c.msgs); got != c.want {
				t.Fatalf("isThinkingOnlyTurn = %v, want %v", got, c.want)
			}
		})
	}
}

// fakeHooks is a programmable inner HookRunner used to verify that steerHooks respect inner
// decisions.
type fakeHooks struct {
	prevent  bool
	blocking []string
	msg      string
}

func (f fakeHooks) PreToolUse(context.Context, string, []byte) (bool, string, []byte) {
	return false, "", nil
}
func (f fakeHooks) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (f fakeHooks) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return f.prevent, f.blocking, f.msg
}

func TestSteerHooksStopNudgesEmptyTurn(t *testing.T) {
	empty := []llm.Message{assistantThinking("I should have listed the subdomain name first.")}

	t.Run("An empty turn in with a repeat command.", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges, label: "worker-1 · #1"}
		prevent, blocking, _ := h.Stop(context.Background(), empty)
		if prevent {
			t.Fatal("An empty turn should not be stopped")
		}
		if len(blocking) != 1 || blocking[0] != emptyTurnNudge {
			t.Fatalf("blocking = %v, want [emptyTurnNudge]", blocking)
		}
	})

	t.Run("Do not intervene when there's a text or tool.", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		normal := []llm.Message{{
			Role:    llm.RoleAssistant,
			Content: []llm.ContentBlock{llm.TextBlock("Scan complete, open port without finding")},
		}}
		if _, blocking, _ := h.Stop(context.Background(), normal); blocking != nil {
			t.Fatalf("Normal closing misconstrued as empty: %v", blocking)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("Unaccompanied when not involved, got %d", n)
		}
	})

	t.Run("When the maximum is reached, let's close.", func(t *testing.T) {
		const limit = 5 // The user assigns the "number of empty response retry" to 5
		h := steerHooks{nudges: &atomic.Int64{}, limit: limit}
		for i := 1; i <= limit; i++ {
			if _, blocking, _ := h.Stop(context.Background(), empty); len(blocking) != 1 {
				t.Fatalf("Number %d should remain in the quota, blocking = %v", i, blocking)
			}
		}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("Exceeding the cap is still being injected: %v", blocking)
		}
	})

	// The "Number of Empty Response Retries" is set to -1 = shut down this layer, and
	// emptyTurnNudgeLimit is resolved to 0.
	t.Run("Do not intervene when configuration is closed", func(t *testing.T) {
		h := steerHooks{nudges: &atomic.Int64{}, limit: 0}
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("Closed Still Injecting: %v", blocking)
		}
	})

	t.Run("Inner decides not to add when it stops.", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{prevent: true, msg: "Guard, refuse to close."}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		prevent, blocking, msg := h.Stop(context.Background(), empty)
		if !prevent || msg != "Guard, refuse to close." || blocking != nil {
			t.Fatalf("inner hard stop was rewritten: prevent=%v blocking=%v msg=%q", prevent, blocking, msg)
		}
		if n := h.nudges.Load(); n != 0 {
			t.Fatalf("No quota should be consumed when giving inner, got %d", n)
		}
	})

	t.Run("Inner, you're gonna have to do it again and again.", func(t *testing.T) {
		h := steerHooks{inner: fakeHooks{blocking: []string{"Guard's reasons for his renewal."}}, nudges: &atomic.Int64{}, limit: defaultEmptyTurnNudges}
		_, blocking, _ := h.Stop(context.Background(), empty)
		if len(blocking) != 1 || blocking[0] != "Guard's reasons for his renewal." {
			t.Fatalf("Inner's follow-up message rewrited: %v", blocking)
		}
	})

	t.Run("No change in behaviour when the counter is not loaded", func(t *testing.T) {
		h := steerHooks{limit: defaultEmptyTurnNudges} // For example, other calls to the future forgot to send nudges
		if _, blocking, _ := h.Stop(context.Background(), empty); blocking != nil {
			t.Fatalf("%v Shouldn't be injected when the calculator is empty:", blocking)
		}
	})
}
