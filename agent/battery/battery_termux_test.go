//go:build linux

package battery

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTermuxScript creates a fake termux-battery-status executable in a
// temporary PATH directory and returns the directory.
func writeTermuxScript(t *testing.T, dir string, content string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "termux-battery-status")
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

const validTermuxJSON = `{"percentage":82.6,"status":"CHARGING","plugged":"PLUGGED_AC","temperature":31.0}
`

// TestTermuxFallbackActive verifies the full fallback flow: sysfs denied +
// TERMUX_VERSION set + fake termux-battery-status on PATH → first call
// fetches synchronously and starts the refresher; later calls use the
// cached snapshot without re-exec.
func TestTermuxFallbackActive(t *testing.T) {
	tmp := t.TempDir()
	batteryRoot = filepath.Join(tmp, "missing_power_supply")
	t.Cleanup(func() { batteryRoot = "/sys/class/power_supply" })
	t.Setenv("TERMUX_VERSION", "0.118.0")
	resetTermuxBatteryCache()
	t.Cleanup(resetTermuxBatteryCache)

	pathDir := writeTermuxScript(t, filepath.Join(tmp, "bin"), "#!/bin/sh\nprintf '%s' '"+validTermuxJSON+"'\n")
	t.Setenv("PATH", pathDir)

	batteries, err := GetBatteryStats()
	if err != nil {
		t.Fatalf("expected termux fallback to provide a battery, got %v", err)
	}
	if len(batteries) != 1 {
		t.Fatalf("expected 1 battery, got %d", len(batteries))
	}
	b := batteries[0]
	if b.Name != "Battery" {
		t.Fatalf("expected name Battery, got %q", b.Name)
	}
	if b.Percent != 83 {
		t.Fatalf("expected percent 83 (82.6 rounded), got %d", b.Percent)
	}
	if b.State != stateCharging {
		t.Fatalf("expected charging state, got %d", b.State)
	}
	if !b.System {
		t.Fatal("expected system battery")
	}
	if !HasReadableBattery() {
		t.Fatal("expected HasReadableBattery to be true")
	}

	// Second call must use the cached snapshot: rewrite the script to fail
	// and confirm the cache still serves a reading.
	writeTermuxScript(t, pathDir, "#!/bin/sh\nexit 1\n")
	cached, err := GetBatteryStats()
	if err != nil {
		t.Fatalf("expected cached snapshot after helper failure, got %v", err)
	}
	if len(cached) != 1 || cached[0].Percent != 83 {
		t.Fatalf("expected cached 83%%, got %+v", cached)
	}
}

// TestTermuxRefresherUpdatesAndBreaks exercises refreshOnce directly:
// success updates the snapshot and resets failures; three consecutive
// failures trip the circuit breaker and stop the refresher.
func TestTermuxRefresherUpdatesAndBreaks(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TERMUX_VERSION", "0.118.0")
	resetTermuxBatteryCache()
	t.Cleanup(resetTermuxBatteryCache)

	calls := filepath.Join(tmp, "calls")
	script := "#!/bin/sh\nprintf x >> " + filepath.Join(calls) + "\nexit 1\n"
	pathDir := writeTermuxScript(t, filepath.Join(tmp, "bin"), script)
	path := filepath.Join(pathDir, "termux-battery-status")

	// Save and restore the package-level fetch hook so later tests exercise
	// the real exec path.
	oldFetch := termuxBatteryFetch
	t.Cleanup(func() { termuxBatteryFetch = oldFetch })

	c := &termuxBatteryCache{}
	// One success first, so the cache has a reading to fall back on.
	termuxBatteryFetch = func(string) ([]Battery, error) {
		return []Battery{{Name: "Battery", Percent: 50, State: stateDischarging, System: true}}, nil
	}
	c.refreshOnce(path)
	if len(c.batteries) != 1 || c.batteries[0].Percent != 50 {
		t.Fatalf("expected refreshed 50%% reading, got %+v", c.batteries)
	}

	// Now point the hook at the failing script and refresh 3 times.
	termuxBatteryFetch = func(string) ([]Battery, error) { return nil, errNoBatteries }
	for i := 0; i < termuxBatteryMaxFailures; i++ {
		c.refreshOnce(path)
	}
	if !c.disabled {
		t.Fatal("expected circuit breaker to disable after repeated failures")
	}
	if c.failures < termuxBatteryMaxFailures {
		t.Fatalf("expected >=%d recorded failures, got %d", termuxBatteryMaxFailures, c.failures)
	}
	// The refresher loop must stop when disabled (simulated by the caller
	// checking c.disabled after refreshOnce).
	if data, err := os.ReadFile(calls); err == nil {
		if len(data) > termuxBatteryMaxFailures {
			t.Fatalf("expected at most %d refresh attempts, got %d", termuxBatteryMaxFailures, len(data))
		}
	}
	// getTermuxBatteryStats must refuse once disabled.
	if _, err := getTermuxBatteryStats(); err == nil {
		t.Fatal("expected error from disabled termux battery collection")
	}
}

// TestTermuxFallbackSkippedOutsideTermux verifies that without
// TERMUX_VERSION nothing is executed and the underlying sysfs error is
// returned by GetBatteryStats.
func TestTermuxFallbackSkippedOutsideTermux(t *testing.T) {
	tmp := t.TempDir()
	batteryRoot = filepath.Join(tmp, "missing_power_supply")
	t.Cleanup(func() { batteryRoot = "/sys/class/power_supply" })
	t.Setenv("TERMUX_VERSION", "")
	resetTermuxBatteryCache()
	t.Cleanup(resetTermuxBatteryCache)

	writeTermuxScript(t, filepath.Join(tmp, "bin"), "#!/bin/sh\nexit 0\n")

	_, err := GetBatteryStats()
	if err == nil {
		t.Fatal("expected error when sysfs is unavailable and TERMUX_VERSION is unset")
	}
	if _, ok := getTermuxBatteryStats(); ok == nil {
		// getTermuxBatteryStats itself must also refuse without TERMUX_VERSION.
		t.Fatal("expected termux collector to skip without TERMUX_VERSION")
	}
}

// TestParseTermuxBattery covers the JSON parsing and state mapping table.
func TestParseTermuxBattery(t *testing.T) {
	cases := []struct {
		name    string
		json    string
		percent uint8
		state   uint8
		wantErr bool
	}{
		{"rounding", `{"percentage":82.6,"status":"DISCHARGING"}`, 83, stateDischarging, false},
		{"clamp high", `{"percentage":150,"status":"FULL"}`, 100, stateFull, false},
		{"clamp low", `{"percentage":-5,"status":"EMPTY"}`, 0, stateEmpty, false},
		{"not charging", `{"percentage":85,"status":"NOT_CHARGING"}`, 85, stateIdle, false},
		{"unknown status", `{"percentage":85,"status":"WEIRD"}`, 85, stateUnknown, false},
		{"missing percentage", `{"status":"CHARGING"}`, 0, 0, true},
		{"bad json", `not json`, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			battery, err := parseTermuxBattery([]byte(tc.json))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %s", tc.json)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if battery.Percent != tc.percent {
				t.Fatalf("expected percent %d, got %d", tc.percent, battery.Percent)
			}
			if battery.State != tc.state {
				t.Fatalf("expected state %d, got %d", tc.state, battery.State)
			}
		})
	}
}

// TestTermuxInitialFetchFailureBreaks verifies the circuit breaker across
// initial (synchronous) fetches: three failing polls disable collection.
func TestTermuxInitialFetchFailureBreaks(t *testing.T) {
	tmp := t.TempDir()
	batteryRoot = filepath.Join(tmp, "missing_power_supply")
	t.Cleanup(func() { batteryRoot = "/sys/class/power_supply" })
	t.Setenv("TERMUX_VERSION", "0.118.0")
	resetTermuxBatteryCache()
	t.Cleanup(resetTermuxBatteryCache)

	calls := filepath.Join(tmp, "calls")
	script := "#!/bin/sh\nprintf x >> " + filepath.Join(calls) + "\nprintf 'garbage'\n"
	pathDir := writeTermuxScript(t, filepath.Join(tmp, "bin"), script)
	t.Setenv("PATH", pathDir)

	for i := 0; i < termuxBatteryMaxFailures; i++ {
		if _, err := GetBatteryStats(); err == nil {
			t.Fatal("expected failure while helper outputs garbage")
		}
	}
	if data, err := os.ReadFile(calls); err != nil || len(data) != termuxBatteryMaxFailures {
		t.Fatalf("expected exactly %d exec attempts, got %v", termuxBatteryMaxFailures, data)
	}
	// After the breaker trips, no further exec may happen.
	writeTermuxScript(t, pathDir, "#!/bin/sh\nprintf x >> "+filepath.Join(calls)+"\nprintf '%s' '"+validTermuxJSON+"'\n")
	if _, err := GetBatteryStats(); err == nil {
		t.Fatal("expected disabled breaker to keep refusing")
	}
	if data, _ := os.ReadFile(calls); len(data) != termuxBatteryMaxFailures {
		t.Fatalf("breaker must stop all exec attempts; calls=%d", len(data))
	}
}

// TestRunTermuxCommand exercises the raw syscall.StartProcess runner that
// replaced os/exec: os/exec probes pidfd_open before the first child start,
// which Android < 12 seccomp answers with a fatal SIGSYS.
func TestRunTermuxCommand(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available on this host")
	}
	ctx := context.Background()

	// stdout capture
	out, err := runTermuxCommand(ctx, "/bin/sh", "-c", "printf 'hello beszel'")
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if string(out) != "hello beszel" {
		t.Fatalf("expected %q, got %q", "hello beszel", out)
	}

	// nonzero exit: stderr tail must surface in the error
	_, err = runTermuxCommand(ctx, "/bin/sh", "-c", "echo boom >&2; exit 3")
	if err == nil || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected exit-status error carrying stderr, got %v", err)
	}

	// timeout: SIGKILL + reap; must return promptly and not leak the child
	timeoutCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err = runTermuxCommand(timeoutCtx, "/bin/sh", "-c", "sleep 30")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout not enforced (took %s)", elapsed)
	}

	// shebang script (how the real termux-battery-status is executed)
	script := filepath.Join(t.TempDir(), "termux-battery-status")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' '"+validTermuxJSON+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err = runTermuxCommand(ctx, script)
	if err != nil {
		t.Fatalf("expected shebang script success, got %v", err)
	}
	if !strings.Contains(string(out), "percentage") {
		t.Fatalf("expected JSON output, got %q", out)
	}
}

// TestRunTermuxCommandNotFound verifies a missing binary produces an error
// (not a crash) from the raw spawn path.
func TestRunTermuxCommandNotFound(t *testing.T) {
	_, err := runTermuxCommand(context.Background(), "/nonexistent/binary/path")
	if err == nil {
		t.Fatal("expected error for missing binary")
	}
	if !strings.Contains(err.Error(), "start") {
		t.Fatalf("expected start error, got %v", err)
	}
}
