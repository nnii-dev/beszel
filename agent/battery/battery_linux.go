//go:build linux || android

package battery

import (
        "log/slog"
        "os"
        "path/filepath"
        "strconv"

        "github.com/henrygd/beszel/agent/utils"
)

var batteryRoot = "/sys/class/power_supply"

// HasReadableBattery reports whether collection currently finds a readable battery.
func HasReadableBattery() bool {
        batteries, _ := GetBatteryStats()
        return len(batteries) > 0
}

func parseSysfsState(status string) uint8 {
        switch status {
        case "Empty":
                return stateEmpty
        case "Full":
                return stateFull
        case "Charging":
                return stateCharging
        case "Discharging":
                return stateDischarging
        case "Not charging":
                return stateIdle
        default:
                return stateUnknown
        }
}

// getSysfsBatteryStats enumerates /sys/class/power_supply and returns every
// readable battery. On unrooted Android the whole directory is SELinux-denied
// to the agent's UID, so this fails and the Termux fallback in
// battery_termux.go is used instead.
func getSysfsBatteryStats() ([]Battery, error) {
        entries, err := os.ReadDir(batteryRoot)
        if err != nil {
                return nil, err
        }
        batteries := make([]Battery, 0, len(entries))
        for _, entry := range entries {
                path := filepath.Join(batteryRoot, entry.Name())
                if utils.ReadStringFile(filepath.Join(path, "type")) != "Battery" {
                        continue
                }
                capStr, ok := utils.ReadStringFileOK(filepath.Join(path, "capacity"))
                if !ok {
                        continue
                }
                cap, parseErr := strconv.Atoi(capStr)
                if parseErr != nil {
                        continue
                }
                cap = min(max(cap, 0), 100)
                name := utils.ReadStringFile(filepath.Join(path, "model_name"))
                if name == "" {
                        name = utils.ReadStringFile(filepath.Join(path, "model"))
                }
                if name == "" {
                        name = entry.Name()
                }
                battery := Battery{
                        Name:    name,
                        Percent: uint8(cap),
                        State:   parseSysfsState(utils.ReadStringFile(filepath.Join(path, "status"))),
                        System:  utils.ReadStringFile(filepath.Join(path, "scope")) != "Device",
                }
                for _, fullName := range []string{"charge_full", "energy_full"} {
                        if parsed, ok := utils.ReadUintFile(filepath.Join(path, fullName)); ok && parsed > 0 {
                                battery.FullChargeCapacity = parsed
                                battery.HasFullChargeCapacity = true
                                break
                        }
                }
                batteries = append(batteries, battery)
        }
        if len(batteries) == 0 {
                return nil, errNoBatteries
        }
        return normalizeBatteries(batteries), nil
}

// GetBatteryStats returns every readable battery, preferring the standard
// sysfs power-supply class and falling back to the Termux battery API
// (termux-battery-status) on unrooted Android, where sysfs is inaccessible.
func GetBatteryStats() ([]Battery, error) {
        batteries, sysfsErr := getSysfsBatteryStats()
        if sysfsErr == nil && len(batteries) > 0 {
                return batteries, nil
        }
        if termuxBatteries, termuxErr := getTermuxBatteryStats(); termuxErr == nil {
                return termuxBatteries, nil
        }
        if sysfsErr != nil {
                slog.Debug("battery: sysfs power supplies unavailable", "err", sysfsErr)
                return nil, sysfsErr
        }
        return nil, errNoBatteries
}
