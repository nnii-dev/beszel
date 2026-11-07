//go:build linux || android

package battery

// Termux battery fallback for unrooted Android.
//
// On an unrooted Android device, /sys/class/power_supply is SELinux-denied
// to the agent's UID, so getSysfsBatteryStats can never find a battery. The
// Termux:API app can read the battery through Android's BatteryManager
// without root; the `termux-battery-status` helper (termux-api package)
// prints it as JSON, e.g.:
//
//      {"percentage":85.0,"status":"NOT_CHARGING","plugged":"UNPLUGGED",...}
//
// Design: the first call after startup fetches synchronously (so the very
// first poll the hub sees already carries battery data) and starts a
// background refresher that updates a cached snapshot every 2 minutes. All
// later polls return the snapshot — no helper process is ever spawned while
// the agent's collection lock is held after startup.
//
// Spawn mechanism: the helper is started with runTermuxCommand, a raw
// syscall.StartProcess runner. os/exec is deliberately avoided: its
// Cmd.Start probes the pidfd_open syscall once per process, and on
// Android < 12 that probe is killed by seccomp with SIGSYS, terminating the
// whole agent (Go issue #69065 — the runtime only auto-mitigates for
// GOOS=android builds, while this agent is built with GOOS=linux).

import (
        "context"
        "encoding/json"
        "errors"
        "fmt"
        "io"
        "log/slog"
        "math"
        "os"
        "path/filepath"
        "strings"
        "sync"
        "syscall"
        "time"

        "github.com/henrygd/beszel/agent/utils"
)

const (
        // termuxBatteryBin is the Termux:API helper that reports battery state
        // as JSON without root.
        termuxBatteryBin = "termux-battery-status"
        // termuxBatteryTimeout bounds a single termux-battery-status invocation;
        // it starts a Java process and a binder roundtrip, so it can take a while.
        termuxBatteryTimeout = 12 * time.Second
        // termuxBatteryRefreshInterval is how often the background refresher
        // updates the cached reading. Battery percentage moves slowly and each
        // refresh spawns a Java process, so this is deliberately slow.
        termuxBatteryRefreshInterval = 2 * time.Minute
        // termuxBatteryMaxFailures is the consecutive-failure circuit breaker
        // threshold after which collection is disabled until restart.
        termuxBatteryMaxFailures = 3
)

// Hooks so tests can inject a fake lookup / fetch.
var (
        termuxBatteryLookPath = utils.SafeLookPath
        termuxBatteryRefresh  = termuxBatteryRefreshInterval
        termuxBatteryFetch    = fetchTermuxBattery
        // termuxBatteryRunner is the process-spawn function used by
        // fetchTermuxBattery; overridable so tests can exercise the real runner
        // without touching a real helper install.
        termuxBatteryRunner = runTermuxCommand
)

// termuxBatteryCache caches the latest reading so getSystemStats never execs
// a helper process under the agent lock after the initial fetch.
type termuxBatteryCache struct {
        mu        sync.Mutex
        batteries []Battery
        at        time.Time
        started   bool // refresher goroutine launched
        failures  int  // consecutive failures (circuit breaker)
        disabled  bool // give up until process restart
}

var termuxCache termuxBatteryCache

// resetTermuxBatteryCache resets all package-level state (tests only).
func resetTermuxBatteryCache() {
        termuxCache.mu.Lock()
        defer termuxCache.mu.Unlock()
        termuxCache.batteries = nil
        termuxCache.at = time.Time{}
        termuxCache.started = false
        termuxCache.failures = 0
        termuxCache.disabled = false
}

// getTermuxBatteryStats returns the Termux battery reading. The first call
// fetches synchronously (logged), and every later call returns the cached
// snapshot that the background refresher keeps current.
func getTermuxBatteryStats() ([]Battery, error) {
        if os.Getenv("TERMUX_VERSION") == "" {
                return nil, errNoBatteries // not running under Termux
        }
        termuxCache.mu.Lock()
        defer termuxCache.mu.Unlock()
        if termuxCache.disabled {
                return nil, errNoBatteries
        }
        if len(termuxCache.batteries) > 0 {
                slog.Debug("termux battery: cached reading",
                        "percent", termuxCache.batteries[0].Percent,
                        "status", termuxCache.batteries[0].State,
                        "age", time.Since(termuxCache.at).Round(time.Second).String())
                return termuxCache.batteries, nil
        }
        // No cached reading yet: fetch one synchronously so the very first poll
        // after the hub connects already carries battery data.
        slog.Info("termux battery: fetching initial reading synchronously")
        path, lookErr := termuxBatteryLookPath(termuxBatteryBin)
        if lookErr != nil {
                // The helper binary is missing; this will not fix itself.
                slog.Warn("termux battery: termux-battery-status not found in PATH", "err", lookErr,
                        "hint", "install the Termux:API app and the termux-api package")
                termuxCache.disabled = true
                return nil, errNoBatteries
        }
        batteries, err := termuxBatteryFetch(path)
        if err != nil {
                termuxCache.recordFailureLocked()
                slog.Debug("termux battery: initial fetch failed", "err", err)
                return nil, err
        }
        termuxCache.batteries = batteries
        termuxCache.at = time.Now()
        termuxCache.failures = 0
        slog.Info("termux battery: fallback active",
                "percent", batteries[0].Percent,
                "status", batteries[0].State,
                "refresh_interval", termuxBatteryRefresh.String())
        termuxCache.startRefresherLocked(path)
        return batteries, nil
}

// recordFailureLocked counts a failed fetch and disables collection once
// termuxBatteryMaxFailures consecutive attempts have failed (circuit
// breaker: never exec a failing helper on every poll).
// Caller must hold c.mu.
func (c *termuxBatteryCache) recordFailureLocked() {
        c.failures++
        if c.failures >= termuxBatteryMaxFailures {
                c.disabled = true
                slog.Warn("termux battery: disabled after repeated failures",
                        "consecutive_failures", c.failures,
                        "hint", "check that the Termux:API app is installed; restart the agent to retry")
        }
}

// startRefresherLocked launches the background refresher goroutine (once).
// Caller must hold c.mu.
func (c *termuxBatteryCache) startRefresherLocked(path string) {
        if c.started {
                return
        }
        c.started = true
        go func() {
                ticker := time.NewTicker(termuxBatteryRefresh)
                defer ticker.Stop()
                for range ticker.C {
                        c.refreshOnce(path)
                        c.mu.Lock()
                        disabled := c.disabled
                        c.mu.Unlock()
                        if disabled {
                                return // circuit breaker tripped: stop refreshing
                        }
                }
        }()
}

// refreshOnce fetches a fresh reading and updates the cache or the failure
// counter. Called from the refresher goroutine only.
func (c *termuxBatteryCache) refreshOnce(path string) {
        batteries, err := termuxBatteryFetch(path)
        c.mu.Lock()
        defer c.mu.Unlock()
        if err != nil {
                c.recordFailureLocked()
                slog.Debug("termux battery: refresh failed", "err", err)
                return
        }
        c.failures = 0
        c.batteries = batteries
        c.at = time.Now()
        slog.Debug("termux battery: refreshed", "percent", batteries[0].Percent, "status", batteries[0].State)
}

// fetchTermuxBattery runs termux-battery-status once and parses its JSON
// output. The resolved path contains a separator and the command is spawned
// with runTermuxCommand (raw syscall.StartProcess), never os/exec — see
// runTermuxCommand for why (pidfd_open probe + seccomp SIGSYS on Android <12).
func fetchTermuxBattery(path string) ([]Battery, error) {
        ctx, cancel := context.WithTimeout(context.Background(), termuxBatteryTimeout)
        defer cancel()
        out, err := termuxBatteryRunner(ctx, path)
        if err != nil {
                return nil, err
        }
        battery, err := parseTermuxBattery(out)
        if err != nil {
                return nil, err
        }
        return normalizeBatteries([]Battery{battery}), nil
}

// runTermuxCommand spawns path with the given arguments and returns the
// child's stdout, bypassing os/exec entirely.
//
// Why not os/exec: exec.Cmd.Start goes through os.startProcess, which calls
// ensurePidfd → checkPidfd. That check probes pidfd_open (kernel 5.3+) once
// per process; on Android < 12 the seccomp filter kills the process with
// SIGSYS for it (Go issue #69065, mitigated upstream only for GOOS=android
// builds — this agent is GOOS=linux). syscall.StartProcess is the plain
// fork/exec path — clone(SIGCHLD) + execve — which every Android seccomp
// filter whitelists, and is exactly what os/exec itself uses once its probe
// fails. (The agent additionally arms the runtime SIGSYS shield; see
// agent/android_seccomp.go. This runner removes os/exec from the critical
// battery path so battery collection works even without it.)
//
// stderr is captured and included in the returned error on nonzero exit.
// ctx caps the runtime: on expiry the child is killed with SIGKILL and
// reaped, so no zombie or goroutine is left behind.
func runTermuxCommand(ctx context.Context, path string, args ...string) ([]byte, error) {
        devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
        if err != nil {
                return nil, fmt.Errorf("%s: open %s: %w", filepath.Base(path), os.DevNull, err)
        }
        defer devnull.Close()

        stdoutR, stdoutW, err := os.Pipe()
        if err != nil {
                return nil, fmt.Errorf("%s: pipe: %w", filepath.Base(path), err)
        }
        stderrR, stderrW, err := os.Pipe()
        if err != nil {
                stdoutR.Close()
                stdoutW.Close()
                return nil, fmt.Errorf("%s: pipe: %w", filepath.Base(path), err)
        }

        attr := &syscall.ProcAttr{
                Env:   os.Environ(),
                Files: []uintptr{devnull.Fd(), stdoutW.Fd(), stderrW.Fd()},
        }
        pid, _, startErr := syscall.StartProcess(path, append([]string{path}, args...), attr)
        // Close the child-side pipe ends in the parent either way so readers see
        // EOF once the child exits.
        stdoutW.Close()
        stderrW.Close()
        if startErr != nil {
                stdoutR.Close()
                stderrR.Close()
                return nil, fmt.Errorf("%s: start: %w", filepath.Base(path), startErr)
        }

        // Drain both pipes; the channels are buffered so the reader goroutines
        // never block on send, even when this function returns early on timeout.
        stdoutCh := make(chan []byte, 1)
        stderrCh := make(chan []byte, 1)
        go func() {
                b, _ := io.ReadAll(stdoutR)
                stdoutR.Close()
                stdoutCh <- b
        }()
        go func() {
                b, _ := io.ReadAll(stderrR)
                stderrR.Close()
                stderrCh <- b
        }()

        type waitResult struct {
                ws  syscall.WaitStatus
                err error
        }
        waitCh := make(chan waitResult, 1)
        go func() {
                var ws syscall.WaitStatus
                for {
                        _, waitErr := syscall.Wait4(pid, &ws, 0, nil)
                        if waitErr == syscall.EINTR {
                                continue
                        }
                        waitCh <- waitResult{ws, waitErr}
                        return
                }
        }()

        var res waitResult
        select {
        case <-ctx.Done():
                syscall.Kill(pid, syscall.SIGKILL)
                res = <-waitCh // reap the killed child
                return nil, fmt.Errorf("%s: timed out after %s", filepath.Base(path), termuxBatteryTimeout)
        case res = <-waitCh:
        }

        stdout := <-stdoutCh
        stderrBytes := <-stderrCh
        if res.err != nil {
                return nil, fmt.Errorf("%s: wait: %w", filepath.Base(path), res.err)
        }
        if res.ws.Signaled() {
                return nil, fmt.Errorf("%s: killed by signal %d", filepath.Base(path), res.ws.Signal())
        }
        if !res.ws.Exited() {
                return nil, fmt.Errorf("%s: did not exit normally (status %#x)", filepath.Base(path), res.ws)
        }
        if code := res.ws.ExitStatus(); code != 0 {
                msg := strings.TrimSpace(string(stderrBytes))
                if msg == "" {
                        msg = strings.TrimSpace(string(stdout))
                }
                if len(msg) > 200 {
                        msg = msg[:200] + "…"
                }
                return nil, fmt.Errorf("%s: exit status %d: %s", filepath.Base(path), code, msg)
        }
        return stdout, nil
}

// parseTermuxBattery parses termux-battery-status JSON, e.g.
// {"percentage":82.5,"status":"DISCHARGING","plugged":"UNPLUGGED",...}.
func parseTermuxBattery(data []byte) (Battery, error) {
        var payload struct {
                Percentage *float64 `json:"percentage"`
                Status     string   `json:"status"`
                Plugged    string   `json:"plugged"`
        }
        if err := json.Unmarshal(data, &payload); err != nil {
                return Battery{}, fmt.Errorf("parsing termux-battery-status output: %w", err)
        }
        if payload.Percentage == nil {
                return Battery{}, errors.New("termux-battery-status: response has no percentage")
        }
        percent := uint8(min(max(math.Round(*payload.Percentage), 0), 100))
        return Battery{
                Name:    "Battery",
                Percent: percent,
                State:   parseTermuxState(payload.Status),
                System:  true,
        }, nil
}

// parseTermuxState maps Android BatteryManager status strings to beszel
// battery states (unknown=0, empty=1, full=2, charging=3, discharging=4,
// idle=5).
func parseTermuxState(status string) uint8 {
        switch status {
        case "CHARGING":
                return stateCharging
        case "DISCHARGING":
                return stateDischarging
        case "FULL":
                return stateFull
        case "NOT_CHARGING":
                return stateIdle
        case "EMPTY":
                return stateEmpty
        default:
                return stateUnknown
        }
}
