package agent

import (
	"log"
	"path/filepath"

	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/noaadapter"
)

// noaWarn returns a diagnostics sink tagging non-fatal noa messages with the
// session, routed through the package logger (agents have no per-instance one).
func noaWarn(session string) func(string) {
	return func(msg string) { log.Printf("[noa] %s: %s", session, msg) }
}

// noa is a model-driven context compression mechanism introduced in norma v0.4.0, which is to be
// switched by the user in the system settings as a platform experiment. It is exclusive to built-in
// compaction: noaadapter.Enable is the only entry, once attached the Compactor, the Compress tool
// and three persistent prompts, without calling Enable is off (built-in compaction works as usual).
// The switch is parsed by the noaEnabledFn injected by each agent, read once per run, so the
// switching only affects the next started run, without rebuilding the agent.

// enableNoa When the parser report opens, connect noa to opts.archiveRoot is the persistent root of
// the compressed source (take the global workDir, each agent is under <workDir>/noa, not dispersed
// with task/intent directories), and sessionID names the archive subdirectory under it (globally
// unique, so there are no conflicts within the same root).
//
// noa is an experimental feature: access failures should not interrupt real tasks. Errors are
// reported by onWarn and return to built-in compression. Clear opts.Compaction when enabled to
// avoid agentcore warnings about "two context managers set at the same time".
func enableNoa(opts *agentcore.Options, enabled func() bool, archiveRoot, sessionID string, onWarn func(string)) {
	if enabled == nil || !enabled() {
		return
	}
	if opts.OnWarn == nil {
		opts.OnWarn = onWarn
	}
	if err := noaadapter.Enable(opts, noaadapter.Options{
		ArchiveBaseDir: filepath.Join(archiveRoot, "noa"),
		SessionID:      sessionID,
		OnWarn:         onWarn,
	}); err != nil {
		if onWarn != nil {
			onWarn("noa 压缩启用失败,回退内置压缩:" + err.Error())
		}
		return
	}
	// Compactor overrides Compaction, but when both coexist agentcore warns each time; clear it
	// explicitly.
	opts.Compaction = nil
}
