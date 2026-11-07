//go:build linux

package agent

// Seccomp SIGSYS shield for unrooted Android.
//
// Modern Go runtimes probe newer syscalls the first time they are needed:
//
//   - os/exec (and os.StartProcess / os.ForkExec) probe pidfd_open (kernel
//     5.3+) once per process, before the first child is started;
//   - exec.LookPath probes faccessat2 (kernel 5.8+).
//
// On Android < 12 the app seccomp filter kills the process with SIGSYS for
// every syscall it does not whitelist (Go issue #69065). The Go runtime ships
// a built-in mitigation for exactly this: setting the runtime's sigsysIgnored
// flag makes sighandler silently swallow seccomp-generated SIGSYS and turn
// the blocked syscall into an ordinary error return, which callers fall back
// from gracefully. The runtime only enables it for GOOS=android builds
// (src/os/pidfd_linux.go, checkPidfd: "In Android version < 12, pidfd-related
// system calls are not allowed by seccomp and trigger the SIGSYS signal").
//
// This agent must be built with GOOS=linux: both the Android CPU fallback and
// the Termux battery fallback are gated on runtime.GOOS == "linux", so the
// runtime's own mitigation never engages. Without this shield the first exec
// in the process — the synchronous initial termux-battery-status fetch inside
// NewAgent — died as:
//
//	SIGSYS: bad system call
//	internal/syscall/unix.PidFDOpen(...)
//	os.checkPidfd() / os.ensurePidfd(...)
//
// The linkname below pulls the runtime's ignoreSIGSYS implementation (pushed
// to os.ignoreSIGSYS in src/runtime/signal_unix.go) so the agent can arm the
// same protection itself. Pulling is permitted by the linker because the
// runtime defines the symbol with a push //go:linkname. Once armed,
// seccomp-trapped syscalls fail with ENOSYS/EPERM instead of killing the
// process, which also protects every other exec site in the agent (network
// monitor pings, smartctl, package updates, ...) on old Android devices.

import (
	"log/slog"
	"os"
	"runtime"
	"sync"
	_ "unsafe" // for //go:linkname
)

//go:linkname ignoreSIGSYS os.ignoreSIGSYS
func ignoreSIGSYS()

var (
	seccompShieldOnce  sync.Once
	seccompShieldArmed bool
)

// armAndroidSeccompShield permanently ignores seccomp-generated SIGSYS
// signals when the agent runs on unrooted Android. It is a no-op on regular
// Linux hosts (TERMUX_VERSION unset and /system/build.prop absent), so server
// behavior is unchanged.
//
// It MUST run before the first os/exec / os.StartProcess in the process:
// NewAgent fetches the initial battery reading synchronously, and that exec
// is what used to trip the runtime's one-time pidfd_open probe.
func armAndroidSeccompShield() {
	if runtime.GOOS != "linux" {
		return
	}
	seccompShieldOnce.Do(func() {
		if !androidRuntimeDetected() {
			return
		}
		ignoreSIGSYS()
		seccompShieldArmed = true
		slog.Info("android seccomp shield armed: seccomp SIGSYS traps are ignored process-wide",
			"note", "syscalls not whitelisted by old Android (pidfd_open, faccessat2, ...) now fail with an error instead of killing the agent")
	})
}

// androidRuntimeDetected reports whether this process runs on Android,
// inside Termux (TERMUX_VERSION is exported by the Termux shell) or under
// any other Android shell (/system/build.prop is world-readable).
func androidRuntimeDetected() bool {
	if os.Getenv("TERMUX_VERSION") != "" {
		return true
	}
	if _, err := os.Stat("/system/build.prop"); err == nil {
		return true
	}
	return false
}
