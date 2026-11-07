//go:build linux

package agent

import (
	"os"
	"testing"
)

// TestAndroidSeccompShieldDetection verifies the Android detection gate:
// TERMUX_VERSION is the primary marker exported by the Termux shell.
func TestAndroidSeccompShieldDetection(t *testing.T) {
	t.Setenv("TERMUX_VERSION", "0.118.0")
	if !androidRuntimeDetected() {
		t.Fatal("expected Android detection via TERMUX_VERSION")
	}
}

// TestAndroidSeccompShieldNoop verifies that on a regular Linux host (no
// TERMUX_VERSION, no /system/build.prop) the shield stays unarmed and
// arming is a harmless no-op.
func TestAndroidSeccompShieldNoop(t *testing.T) {
	t.Setenv("TERMUX_VERSION", "")
	if _, err := os.Stat("/system/build.prop"); err == nil {
		t.Skip("/system/build.prop readable: this looks like a real Android host")
	}
	if androidRuntimeDetected() {
		t.Fatal("expected no Android detection on a regular Linux host")
	}
	armAndroidSeccompShield()
	if seccompShieldArmed {
		t.Fatal("shield must stay unarmed off Android")
	}
}
