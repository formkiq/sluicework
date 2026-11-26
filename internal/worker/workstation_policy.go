package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

type WorkstationConfig struct {
	RunnerDir            string  `json:"runnerDir"`
	MaxLoadPerCPU        float64 `json:"maxLoadPerCpu,omitempty"`
	MinAvailableMemoryMB int     `json:"minAvailableMemoryMb,omitempty"`
	MinIdleSeconds       *int    `json:"minIdleSeconds,omitempty"`
	RequireACPower       *bool   `json:"requireAcPower,omitempty"`
	BuildThreads         int     `json:"buildThreads,omitempty"`
	EnableMinutes        int     `json:"enableMinutes,omitempty"`
}

type hostResources struct {
	Load        float64
	CPUs        int
	AvailableMB int
	IdleSeconds int
	AC          bool
	ThermalOK   bool
}

func (w WorkstationConfig) defaults() WorkstationConfig {
	if w.MaxLoadPerCPU == 0 {
		w.MaxLoadPerCPU = 0.5
	}
	if w.MinAvailableMemoryMB == 0 {
		w.MinAvailableMemoryMB = 4096
	}
	if w.BuildThreads == 0 {
		w.BuildThreads = 2
	}
	if w.EnableMinutes == 0 {
		w.EnableMinutes = 120
	}
	if w.MinIdleSeconds == nil {
		v := 0
		if runtime.GOOS == "darwin" {
			v = 120
		}
		w.MinIdleSeconds = &v
	}
	if w.RequireACPower == nil {
		v := runtime.GOOS == "darwin"
		w.RequireACPower = &v
	}
	return w
}

func (w WorkstationConfig) validate() error {
	if w.MaxLoadPerCPU <= 0 || w.MaxLoadPerCPU > 1 || w.MinAvailableMemoryMB < 512 || w.BuildThreads < 1 || w.BuildThreads > 8 || w.EnableMinutes < 1 || w.EnableMinutes > 1440 || *w.MinIdleSeconds < 0 || *w.MinIdleSeconds > 86400 {
		return errors.New("invalid workstation resource limits (1–8 build threads, 1–1440 enabled minutes)")
	}
	if runtime.GOOS == "linux" && *w.MinIdleSeconds > 0 {
		return errors.New("Linux input-idle detection is unavailable; explicitly set minIdleSeconds to 0")
	}
	return nil
}

func (w WorkstationConfig) eligible(h hostResources) string {
	switch {
	case h.CPUs <= 0 || h.AvailableMB < 0 || h.Load < 0 || h.IdleSeconds < 0:
		return "resource checks unavailable"
	case !h.ThermalOK:
		return "thermal pressure"
	case *w.RequireACPower && !h.AC:
		return "waiting for AC power"
	case h.AvailableMB < w.MinAvailableMemoryMB:
		return "waiting for available memory"
	case h.Load/float64(h.CPUs) > w.MaxLoadPerCPU:
		return "waiting for CPU capacity"
	case h.IdleSeconds < *w.MinIdleSeconds:
		return "foreground activity"
	default:
		return ""
	}
}

func probeCommand(ctx context.Context, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, args...).Output()
	return string(b), err
}

var decimal = regexp.MustCompile(`[0-9]+(?:\.[0-9]+)?`)
var idleValue = regexp.MustCompile(`"HIDIdleTime"\s*=\s*([0-9]+)`)
var memoryPercent = regexp.MustCompile(`System-wide memory free percentage:\s*([0-9]+)%`)
var speedLimit = regexp.MustCompile(`CPU_Speed_Limit\s*=\s*([0-9]+)`)

func resources(ctx context.Context, w WorkstationConfig) (hostResources, error) {
	h := hostResources{CPUs: runtime.NumCPU(), AC: true, ThermalOK: true}
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/loadavg")
		if err != nil {
			return h, err
		}
		h.Load, err = strconv.ParseFloat(strings.Fields(string(b))[0], 64)
		if err != nil {
			return h, err
		}
		b, err = os.ReadFile("/proc/meminfo")
		if err != nil {
			return h, err
		}
		h.AvailableMB = -1
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "MemAvailable:") {
				var kb int
				_, err = fmt.Sscanf(line, "MemAvailable: %d kB", &kb)
				if err != nil {
					return h, err
				}
				h.AvailableMB = kb / 1024
			}
		}
		if *w.RequireACPower {
			h.AC = false
			files, _ := os.ReadDir("/sys/class/power_supply")
			for _, f := range files {
				b, _ := os.ReadFile("/sys/class/power_supply/" + f.Name() + "/online")
				if strings.TrimSpace(string(b)) == "1" {
					h.AC = true
				}
			}
		}
		return h, nil
	}
	if runtime.GOOS != "darwin" {
		return h, errors.New("workstation requires macOS or Linux")
	}
	load, err := probeCommand(ctx, "/usr/sbin/sysctl", "-n", "vm.loadavg")
	if err != nil {
		return h, err
	}
	n := decimal.FindString(load)
	if n == "" {
		return h, errors.New("missing CPU load")
	}
	h.Load, _ = strconv.ParseFloat(n, 64)
	mem, err := probeCommand(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize")
	if err != nil {
		return h, err
	}
	bytes, err := strconv.ParseInt(strings.TrimSpace(mem), 10, 64)
	if err != nil {
		return h, err
	}
	pressure, err := probeCommand(ctx, "/usr/bin/memory_pressure", "-Q")
	if err != nil {
		return h, err
	}
	m := memoryPercent.FindStringSubmatch(pressure)
	if m == nil {
		return h, errors.New("missing memory pressure reading")
	}
	pct, _ := strconv.ParseInt(m[1], 10, 64)
	h.AvailableMB = int(bytes / 100 * pct / (1024 * 1024))
	if *w.RequireACPower {
		power, err := probeCommand(ctx, "/usr/bin/pmset", "-g", "batt")
		if err != nil {
			return h, err
		}
		h.AC = strings.Contains(power, "'AC Power'")
	}
	thermal, err := probeCommand(ctx, "/usr/bin/pmset", "-g", "therm")
	if err != nil {
		return h, err
	}
	if m := speedLimit.FindStringSubmatch(thermal); m != nil {
		limit, _ := strconv.Atoi(m[1])
		h.ThermalOK = limit == 100
	}
	if strings.Contains(thermal, "Thermal_Level") && !strings.Contains(thermal, "Thermal_Level = 0") {
		h.ThermalOK = false
	}
	if *w.MinIdleSeconds > 0 {
		idle, err := probeCommand(ctx, "/usr/sbin/ioreg", "-c", "IOHIDSystem")
		if err != nil {
			return h, err
		}
		m := idleValue.FindStringSubmatch(idle)
		if m == nil {
			return h, errors.New("missing input-idle reading")
		}
		ns, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return h, err
		}
		h.IdleSeconds = int(ns / int64(time.Second))
	}
	return h, nil
}
