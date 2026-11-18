package agent

import (
	"context"
	"errors"
	"fmt"
)

// AbortCause names why an agent run's context was cancelled. Every cancellation
// site should attach one so the activity trace can report the real initiator.
type AbortCause struct {
	Code  string
	Short string
	Text  string
}

func (c *AbortCause) Error() string { return c.Text }

func cause(code, short, text string) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: text}
}

// Causef builds a cause that includes runtime-specific detail.
func Causef(code, short, format string, args ...any) *AbortCause {
	return &AbortCause{Code: code, Short: short, Text: fmt.Sprintf(format, args...)}
}

var (
	// Task-level execution context.
	AbortPausedByUser = cause("paused_by_user", "User paused task",
		"The user has paused the task via the task control interface (POST /api/tasks/{id}/control, action=pause). This Planner/Worker run is actively cancelled; the running intent returns to frontier(open), and the task is reclaimed and executed from scratch when resumed.")
	AbortPausedByOrchestrator = cause("paused_by_orchestrator", "Orchestrator paused task",
		"The orchestration Agent called pause_task. The current Planner and Worker runs are cancelled; running intents return to the open frontier and are executed again after the task resumes.")
	AbortTaskDeleted = cause("task_deleted", "Task was deleted.",
		"The task is being deleted (DELETE /api/tasks/{id}), the deletion barrier has removed the Planner, Worker and Master Agent that the task is running on; the results of this execution will no longer be used")
	AbortPausedOnReload = cause("paused_on_reload", "Backend has restored task's suspension.",
		"The backend resumes the paused task at startup based on the persistent state in the database. This run is cancelled; normally there are no running Agents in the recovery phase")
	AbortGoalMet = cause("goal_met", "Planner marked goal complete",
		"The scheduler determines that the task goal has been met and sets the task to done, then cancels the still running Worker; these intents are marked as stopped rather than failed")
	AbortSettleDrainTimeout = cause("settle_drain_timeout", "Task drain grace expired",
		"When the task reaches timeout, it waits for the running Worker to elegantly end, but the 90-second drain margin is still insufficient and therefore executes a hard cancellation; the intent will be marked as exhausted, and the fact that the end phase has been written and the asset will be retained")

	// Per-work context.
	AbortKilledByPlanner = cause("killed_by_planner", "Planner terminated this intent.",
		"The planner calls kill_work to actively terminate the intent, usually indicating that it is out of direction or no longer has a continuing value; the intent will be marked as stopped and not automatically reclaimed")
	AbortWorkPausedByUser = cause("work_paused_by_user", "User paused Worker intent",
		"The user has paused the running Worker. This call is cancelled and the intent becomes paused; all registered intent, fact, vulnerability, and activity records are retained and resumed from scratch")
	AbortWorkCancelledByUser = cause("work_cancelled_by_user", "User deleted Worker intent",
		"The user deletes the running Worker. This call is cancelled; after the Worker leaves the write zone, the server handles the intent in the deletion mode chosen by the user — false delete only marks it deleted and retains all output, while true delete cascades out the intention and the downstream nodes supported by it alone.")
	AbortWorkFinished = cause("work_finished", "Worker finished normally",
		"Worker has terminated properly, and the engine releases its context resource in detachWork. This is not a run interrupt; if it appears in an interrupt message, it indicates that there was a contest between the cancellation and closure event")
	AbortPausedRaceGuard = cause("paused_race_guard", "Run blocked while task paused",
		"When the task is suspended, the engine refuses to send a new execution context to prevent a contest between the claim and the suspension from causing the Worker to continue launching; the received intent returns to frontier")

	// Main Agent and standalone conversation contexts.
	AbortChatStoppedByUser = cause("chat_stopped_by_user", "The user stopped the conversation.",
		"The user clicks Stop, actively stopping the current run of the Master Agent or Session Agent. The activity record that has been generated is retained and the next message can be continued")
	AbortChatPausedWithTask = cause("chat_paused_with_task", "Task pause stopped main chat",
		"When the user suspends a task, the running main Agent dialog is also desynchronized. Activity records that have been generated are preserved; when the task is resumed, the current round of messages is not automatically replayed")
	AbortChatTurnFinished = cause("chat_turn_finished", "Chat turn finished normally",
		"This is not an interrupt; if it appears in the interrupt message, it indicates that there was a conflict between the cancellation and closure event.")

	// Process-level and per-run hard backstop.
	AbortShutdown = cause("shutdown", "Backend process is closing",
		"The backend process receives a SIGINT or SIGTERM and is restarting, updating, or shutting down. All running Agents are cancelled; the remaining running intent is reset to open after restart and executed again")
	AbortRunHardTimeout = cause("run_hard_timeout", "Run hard timeout triggered",
		"A single run exceeding the softwall clock budget and additional tolerance indicates that a model request or a tool has not returned for a long time, causing the normal loop boundary ending to fail. Focus on checking the last unreturned tool call before interrupting")
)

// AbortReason resolves the named cause attached to a cancelled run context.
func AbortReason(ctx context.Context) (code, short, text string, ok bool) {
	c := context.Cause(ctx)
	if c == nil {
		return "", "", "", false
	}
	var ac *AbortCause
	if errors.As(c, &ac) {
		return ac.Code, ac.Short, ac.Text, true
	}
	switch {
	case errors.Is(c, context.DeadlineExceeded):
		return "deadline_exceeded", "Upstream context to deadline",
			"The upstream context reaches the deadline, but the setter does not add a named reason via WithTimeoutCause: " + c.Error(), true
	case errors.Is(c, context.Canceled):
		return "canceled_no_cause", "Unnamed reason for cancellation",
			"The upstream context was cancelled, but the canceler did not add a named cause through context.WithCancelCause; please register the reason in agent/cancelcause.go and access this cancellation point", true
	default:
		return "other", firstLine(c.Error(), 80), c.Error(), true
	}
}
