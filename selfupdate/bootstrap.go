package selfupdate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// smokeEnv makes the child processes that are pulled by the smoke test skip Bootstrap directly.
//
// Strictly speaking, nothing happens without adding: the child process's os.Executable() is
// artex.new, and the resulting paths all have the.new prefix, so they do not encounter the actual
// upgrade file. But relying on this coincidence is too vulnerable, and it is obvious that it shorts
// out the child process from having to perform one useless disk detection.
const smokeEnv = "ARTEX_SELFUPDATE_SMOKE"

// Action is Bootstrap's command to main.
type Action int

const (
	// Continue: Starts the server as usual.
	Continue Action = iota
	// Restart: Exit immediately with ExitRestart, and let the guard script restart.
	Restart
)

// State describes the status of the upgrade at the current startup, so that /api/update/check can
// tell the front-end "is the last upgrade successful or was rolled back".
type State struct {
	Pending     bool   // Stable after re-fitting not yet confirmed
	RolledBack  bool   // This startup has just performed an automatic rollback
	FailedStage bool   // Unsuccessful in validation/smoking and discarded
	Detail      string // One-phrase description for the user
}

// Bootstrap runs at the very beginning of main and must be called before any listening ports or
// database openings.
//
// Three scenarios:
//
//	① There is a temporary file artex.new → Check + Smoke, if it passes, change and request restart;
//	if it fails, discard and continue running the old version ② Only the marked files remain →
//	Indicate that you just changed, cumulatively one attempt; if you fail enough times in a row,
//	roll back ③ Nothing → Normal startup
func Bootstrap() (Action, State) {
	if os.Getenv(smokeEnv) != "" {
		return Continue, State{}
	}
	p, err := ResolvePaths()
	if err != nil {
		log.Printf("[update] Skipping self-inflicted: %v", err)
		return Continue, State{}
	}

	if _, err := os.Stat(p.New); err == nil {
		return applyStaged(p)
	}

	m, ok := readMarker(p.Marker)
	if !ok {
		return Continue, State{}
	}
	return confirmOrRollback(p, m)
}

// applyStaged Handles "available staging" situations: validation passes, reloading, failure
// discards.
//
// This is the only place where the entire upgrade link covers the executable, and it is the last
// gate – smoke testing prevents download corruption, architectural errors, and missing dynamic
// links. Once you release a binary that doesn’t run, the guard script will tirelessly pull it over
// and over again, and the Go code won’t have a chance to run, so there’s no automatic rollback.
func applyStaged(p Paths) (Action, State) {
	m, _ := readMarker(p.Marker)

	if err := verifyStaged(p); err != nil {
		log.Printf("[update] New version of the pending version has not been verified, abandoned and continues with current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "Failed to verify new version, discarded" + err.Error()}
	}

	if err := swap(p); err != nil {
		log.Printf("[update] Change failed to run current version: %v", err)
		cleanStaged(p)
		_ = os.Remove(p.Marker)
		return Continue, State{FailedStage: true, Detail: "_Other Organiser" + err.Error()}
	}

	// The changeover was successful. Keep the mark and confirm the stability at the next boot
	// (running the new version).
	m.Attempts = 0
	if m.StagedAt == 0 {
		m.StagedAt = time.Now().Unix()
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] Writing upgrade mark failed (lost automatic rollback capacity): %v", err)
	}
	log.Printf("[update] Changed to %s, exited to restart (exit %d)", orUnknown(m.To), ExitRestart)
	return Restart, State{Pending: true}
}

// confirmOrRollback Handles "rebooted" boots: accumulates the number of attempts, and rolls back
// the old version if the limit is exceeded.
//
// The count increases only after the Go code is run, so it covers "executable but crashed on
// initialization" errors (configuration incompatible, port occupied, DB migration crashes); "exec
// simply not possible" is blocked by smoke testing before reloading, and the two are combined to be
// complete.
func confirmOrRollback(p Paths, m marker) (Action, State) {
	m.Attempts++
	if m.Attempts > maxAttempts {
		if err := rollback(p); err != nil {
			// Uncheck, and let the process start as it is — if not, at least let the user see why
			// in the log.
			log.Printf("[update] New version consecutively %d failed to start and rollback failed: %v", maxAttempts, err)
			_ = os.Remove(p.Marker)
			return Continue, State{Detail: "The new version failed to start and roll back:" + err.Error()}
		}
		log.Printf("[update] New version failed to boot for %d consecutive times, rolled back to %s, exited to restart (exit %d)",
			maxAttempts, orUnknown(m.From), ExitRestart)
		_ = os.Remove(p.Marker)
		return Restart, State{RolledBack: true, Detail: fmt.Sprintf("New version startup failed, rolled back to %s", orUnknown(m.From))}
	}
	if err := writeMarker(p.Marker, m); err != nil {
		log.Printf("[update] Update upgrade mark failed: %v", err)
	}
	log.Printf("[update] The new version is being started (at the %d/%d attempt), and will be confirmed for upgrade after steady operation",
		m.Attempts, maxAttempts)
	return Continue, State{Pending: true}
}

// Settle confirms that the new version is stable and removes the upgrade mark.
//
// Delayed by main after HTTP listens: this time is counted, otherwise the marker remains in place,
// and the next start continues to accumulate the number of attempts until a rollback is triggered.
func Settle() {
	p, err := ResolvePaths()
	if err != nil {
		return
	}
	settle(p)
}

func settle(p Paths) {
	if _, ok := readMarker(p.Marker); !ok {
		return // Not an upgraded boot, nothing to do
	}
	if err := os.Remove(p.Marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Printf("[update] Clearing upgrade mark failed: %v", err)
		return
	}
	log.Printf("[update] New version running steady, upgrade complete (previous version retained as %s)", p.Old)
}

// SettleDelay is the runtime required to determine that "the new version is alive."
const SettleDelay = 30 * time.Second

// verifyStaged Checks the cache for SHA256 before actually pulling it up and running it.
func verifyStaged(p Paths) error {
	want, err := os.ReadFile(p.Sum)
	if err != nil {
		return fmt.Errorf("Read checksum: %w", err)
	}
	got, err := fileSHA256(p.New)
	if err != nil {
		return fmt.Errorf("Calculate checksum: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(string(want)), got) {
		return errors.New("SHA256 mismatch (download damaged or tampered)")
	}
	return smokeTest(p.New)
}

// smokeTest starts the new binary with -h to verify that it is actually executable on the current
// system. This prevents download interruptions, exec format errors, missing dependencies, and other
// problems.
func smokeTest(bin string) error {
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("Give executory permissions: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// nosemgrep: dangerous-exec-command -- bin is the staged file whose SHA-256
	// was verified immediately above; no shell is involved.
	cmd := exec.CommandContext(ctx, bin, "-h") // nosemgrep
	cmd.Env = append(os.Environ(), smokeEnv+"=1")
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return errors.New("Smoke testtimeout")
	}
	if err != nil {
		snippet := strings.TrimSpace(string(out))
		if len(snippet) > 300 {
			snippet = snippet[:300] + "…"
		}
		return fmt.Errorf("Smoke test failed: %v: %s", err, snippet)
	}
	return nil
}

// swap Swaps the current binary with a new version of the temporary file.
//
// Both Unix and Windows allow renaming a running executable (Windows only forbids deletion and
// overwriting, rename is not in the list), so there is no need to sub-platform or shut down itself
// first.
func swap(p Paths) error {
	// Windows rename does not overwrite existing targets, and.old left behind from the previous
	// upgrade must be cleared first.
	if err := os.Remove(p.Old); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Clear old backup %s: %w", p.Old, err)
	}
	if err := os.Rename(p.Current, p.Old); err != nil {
		return fmt.Errorf("Backup current version: %w", err)
	}
	if err := os.Rename(p.New, p.Current); err != nil {
		// The reinstall failed but the current version has been removed and must be returned as it
		// is, otherwise there will be no executable on the next startup.
		if rerr := os.Rename(p.Old, p.Current); rerr != nil {
			return fmt.Errorf("Failed to load new version (%v) and to restore current version: %w", err, rerr)
		}
		return fmt.Errorf("Load new version: %w", err)
	}
	_ = os.Remove(p.Sum)
	return nil
}

// rollback rolls back the old version of the swap backup.
func rollback(p Paths) error {
	if _, err := os.Stat(p.Old); err != nil {
		return fmt.Errorf("%s: %w", p.Old, err)
	}
	// Move the new version that failed to.failed for check, rather than delete it directly.
	failed := p.Current + ".failed"
	_ = os.Remove(failed)
	if err := os.Rename(p.Current, failed); err != nil {
		return fmt.Errorf("Remove failed version: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		return fmt.Errorf("Restore old version: %w", err)
	}
	return nil
}

// Rollback is an implementation of /api/update/rollback: actively rollbacks to the previous
// version. Only reinstalls and restarts, as given to the guardian script (the caller then exits
// with ExitRestart).
func Rollback() error {
	p, err := ResolvePaths()
	if err != nil {
		return err
	}
	if _, err := os.Stat(p.Old); err != nil {
		return errors.New("No previous version to roll back (" + p.Old + " (do not exist)")
	}
	cleanStaged(p)
	if err := smokeTest(p.Old); err != nil {
		return fmt.Errorf("Previous version could not be implemented, refusal to roll back: %w", err)
	}
	// Exchange current and backup: roll back and roll back again.
	tmp := p.Current + ".swap"
	_ = os.Remove(tmp)
	if err := os.Rename(p.Current, tmp); err != nil {
		return fmt.Errorf("Move current version: %w", err)
	}
	if err := os.Rename(p.Old, p.Current); err != nil {
		_ = os.Rename(tmp, p.Current)
		return fmt.Errorf("Load previous version: %w", err)
	}
	if err := os.Rename(tmp, p.Old); err != nil {
		log.Printf("[update] Collapse backup failed after rolling back (not affecting running): %v", err)
	}
	_ = os.Remove(p.Marker)
	return nil
}

// HasBackup reports whether there is a previous version that can be rolled back, so the front-end
// decides whether to display the rollback button.
func HasBackup() bool {
	p, err := ResolvePaths()
	if err != nil {
		return false
	}
	_, err = os.Stat(p.Old)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func orUnknown(s string) string {
	if strings.TrimSpace(s) == "" {
		return "Unknown version"
	}
	return s
}
