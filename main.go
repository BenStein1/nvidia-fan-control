package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/NVIDIA/go-nvml/pkg/nvml"
)

type Config struct {
	TimeToUpdate      int                `json:"time_to_update"`
	TemperatureRanges []TemperatureRange `json:"temperature_ranges"`
	Curve             bool               `json:"curve"` // optional; default false => original step behavior
}

type TemperatureRange struct {
	MinTemperature int `json:"min_temperature"`
	MaxTemperature int `json:"max_temperature"`
	FanSpeed       int `json:"fan_speed"`
	Hysteresis     int `json:"hysteresis"`
}

func loadConfig(file string) (Config, error) {
	var config Config
	data, err := os.ReadFile(file)
	if err != nil {
		return config, err
	}
	err = json.Unmarshal(data, &config)
	return config, err
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// Original behavior: step function based on the range that contains temp.
func getFanSpeedForTemperature(temp, prevTemp, prevSpeed int, ranges []TemperatureRange) int {
	for _, r := range ranges {
		if temp > r.MinTemperature && temp <= r.MaxTemperature {
			if abs(temp-prevTemp) >= r.Hysteresis || prevSpeed != r.FanSpeed {
				return r.FanSpeed
			}
			return prevSpeed
		}
	}
	return prevSpeed
}

func setupLogging(logFilePath string) (*os.File, error) {
	logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", logFilePath, err)
	}
	log.SetOutput(logFile)
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("INFO: Logging setup complete.")
	return logFile, nil
}

func loadConfiguration(configPath string) (Config, error) {
	config, err := loadConfig(configPath)
	if err != nil {
		return config, fmt.Errorf("failed to load config %s: %w", configPath, err)
	}

	if config.TimeToUpdate <= 0 {
		log.Printf("WARN: time_to_update (%d) is invalid, defaulting to 5 seconds.", config.TimeToUpdate)
		config.TimeToUpdate = 5
	}

	if len(config.TemperatureRanges) == 0 {
		log.Println("WARN: temperature_ranges is empty.")
	}

	log.Println("INFO: Configuration loaded and validated.")
	return config, nil
}

func initializeNVML() (cleanupFunc func(), err error) {
	ret := nvml.Init()
	if ret != nvml.SUCCESS {
		return nil, fmt.Errorf("unable to initialize NVML: %v", nvml.ErrorString(ret))
	}

	cleanupFunc = func() {
		log.Println("INFO: Shutting down NVML...")
		ret := nvml.Shutdown()
		if ret != nvml.SUCCESS {
			log.Printf("ERROR: Unable to shutdown NVML cleanly: %v", nvml.ErrorString(ret))
		} else {
			log.Println("INFO: NVML Shutdown complete.")
		}
	}

	log.Println("INFO: NVML initialized successfully.")
	return cleanupFunc, nil
}

func initializeDevices() (count int, fanCounts []int, prevTemps []int, prevFanSpeeds [][]int, err error) {
	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		return 0, nil, nil, nil, fmt.Errorf("unable to get NVIDIA device count: %v", nvml.ErrorString(ret))
	}
	if count == 0 {
		return 0, nil, nil, nil, fmt.Errorf("no NVIDIA devices found")
	}
	log.Printf("INFO: Found %d NVIDIA device(s).", count)

	fanCounts = make([]int, count)
	prevTemps = make([]int, count)
	prevFanSpeeds = make([][]int, count)
	initializedDevices := 0

	for i := 0; i < count; i++ {
		device, ret := nvml.DeviceGetHandleByIndex(i)
		if ret != nvml.SUCCESS {
			log.Printf("WARN: Unable to get handle for device %d: %v. Skipping device.", i, nvml.ErrorString(ret))
			fanCounts[i] = 0
			continue
		}

		var numFansInt int
		numFansInt, ret = nvml.DeviceGetNumFans(device)
		if ret != nvml.SUCCESS {
			log.Printf("WARN: Unable to get fan count for device %d: %v. Assuming 0 fans or fan control not supported.", i, nvml.ErrorString(ret))
			fanCounts[i] = 0
			continue
		}
		fanCounts[i] = numFansInt

		if fanCounts[i] <= 0 {
			log.Printf("INFO: Device %d reports %d controllable fans. Skipping fan initialization.", i, fanCounts[i])
			continue
		}

		log.Printf("INFO: Device %d has %d controllable fan(s). Initializing state.", i, fanCounts[i])
		prevFanSpeeds[i] = make([]int, fanCounts[i])

		temp, ret := nvml.DeviceGetTemperature(device, nvml.TEMPERATURE_GPU)
		if ret == nvml.SUCCESS {
			prevTemps[i] = int(temp)
		} else {
			log.Printf("WARN: Failed to get initial temperature for device %d: %v. Using 0.", i, nvml.ErrorString(ret))
			prevTemps[i] = 0
		}

		for fanIdx := 0; fanIdx < fanCounts[i]; fanIdx++ {
			speed, ret := nvml.DeviceGetFanSpeed_v2(device, fanIdx)
			if ret == nvml.SUCCESS {
				prevFanSpeeds[i][fanIdx] = int(speed)
			} else {
				speedLegacy, retLegacy := nvml.DeviceGetFanSpeed(device)
				if retLegacy == nvml.SUCCESS && fanIdx == 0 {
					log.Printf("WARN: Using legacy DeviceGetFanSpeed for initial speed for device %d Fan %d.", i, fanIdx)
					prevFanSpeeds[i][fanIdx] = int(speedLegacy)
				} else {
					log.Printf("WARN: Failed to get initial speed for device %d Fan %d using v2 (%v) or legacy (%v). Using 0.",
						i, fanIdx, nvml.ErrorString(ret), nvml.ErrorString(retLegacy))
					prevFanSpeeds[i][fanIdx] = 0
				}
			}
		}
		log.Printf("INFO: Initial state for device %d: Temp=%d°C, Fan Speeds=%v%%", i, prevTemps[i], prevFanSpeeds[i])
		initializedDevices++
	}

	if initializedDevices == 0 && count > 0 {
		return count, fanCounts, prevTemps, prevFanSpeeds, fmt.Errorf("found %d devices, but failed to initialize any for monitoring/control", count)
	}

	log.Printf("INFO: Device initialization complete. Monitoring %d devices.", initializedDevices)
	return count, fanCounts, prevTemps, prevFanSpeeds, nil
}

// ---------- Curve mode helpers (floor + setpoints + ceiling) ----------

type curvePoint struct {
	temp  int
	speed int
	hyst  int
}

type curveProfile struct {
	floorEndTemp int // temps < floorEndTemp => floorSpeed
	floorSpeed   int
	floorHyst    int
	points       []curvePoint // sorted by temp; curve only between these setpoints; >= last temp => last speed
}

func clampInt(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// New curve semantics:
// - Treat the LOWEST min_temperature range as the "floor range".
//   Use its max_temperature as floorEndTemp, and its fan_speed as floorSpeed.
// - Every OTHER range contributes a setpoint at (min_temperature -> fan_speed) with its hysteresis.
// - Below floorEndTemp: fixed floorSpeed.
// - Between setpoints: linear interpolation.
// - Above last setpoint: fixed at last setpoint speed.
// This matches: "floor + ceiling, smooth only between setpoints".
func buildCurveProfileFromRanges(ranges []TemperatureRange) (curveProfile, error) {
	var prof curveProfile
	if len(ranges) == 0 {
		return prof, fmt.Errorf("no temperature_ranges provided")
	}

	// Sort by min_temperature to identify the floor range.
	rs := make([]TemperatureRange, len(ranges))
	copy(rs, ranges)
	sort.SliceStable(rs, func(i, j int) bool {
		if rs[i].MinTemperature == rs[j].MinTemperature {
			return rs[i].MaxTemperature < rs[j].MaxTemperature
		}
		return rs[i].MinTemperature < rs[j].MinTemperature
	})

	floor := rs[0]
	prof.floorEndTemp = floor.MaxTemperature
	prof.floorSpeed = clampInt(floor.FanSpeed, 0, 100)
	prof.floorHyst = floor.Hysteresis

	pts := make([]curvePoint, 0, len(rs)-1)
	for i := 1; i < len(rs); i++ {
		r := rs[i]
		pts = append(pts, curvePoint{
			temp:  r.MinTemperature,
			speed: clampInt(r.FanSpeed, 0, 100),
			hyst:  r.Hysteresis,
		})
	}

	// Sort points and dedupe by temp (last wins).
	sort.SliceStable(pts, func(i, j int) bool { return pts[i].temp < pts[j].temp })
	dedup := make([]curvePoint, 0, len(pts))
	for _, p := range pts {
		if len(dedup) == 0 {
			dedup = append(dedup, p)
			continue
		}
		if dedup[len(dedup)-1].temp == p.temp {
			dedup[len(dedup)-1] = p
			continue
		}
		dedup = append(dedup, p)
	}
	prof.points = dedup

	// If there are no setpoints, curve mode degenerates to "always floorSpeed".
	return prof, nil
}

// Compute curve speed for temp given profile.
// Returns (speed, hysteresisUsed).
func curveSpeedForTempWithProfile(temp int, prof curveProfile) (int, int) {
	// Floor clamp:
	if temp < prof.floorEndTemp {
		return prof.floorSpeed, prof.floorHyst
	}

	// No setpoints => always floor speed.
	if len(prof.points) == 0 {
		return prof.floorSpeed, prof.floorHyst
	}

	// If we're between floorEndTemp and the first setpoint temp (gap), keep floor.
	if temp < prof.points[0].temp {
		return prof.floorSpeed, prof.floorHyst
	}

	// Ceiling clamp:
	last := prof.points[len(prof.points)-1]
	if temp >= last.temp {
		return last.speed, last.hyst
	}

	// Find segment between setpoints and interpolate.
	for i := 0; i < len(prof.points)-1; i++ {
		a := prof.points[i]
		b := prof.points[i+1]
		if temp >= a.temp && temp < b.temp {
			den := b.temp - a.temp
			if den <= 0 {
				return a.speed, a.hyst
			}
			t := float64(temp-a.temp) / float64(den)
			val := float64(a.speed) + t*float64(b.speed-a.speed)
			speed := int(math.Round(val))
			speed = clampInt(speed, 0, 100)
			return speed, a.hyst
		}
	}

	// Fallback (shouldn't hit)
	return last.speed, last.hyst
}

func runMonitoringLoop(config Config, count int, fanCounts []int, prevTemps []int, prevFanSpeeds [][]int) {
	log.Println("INFO: Starting monitoring loop...")

	var (
		useCurve bool
		prof    curveProfile
	)

	useCurve = config.Curve
	if useCurve {
		var err error
		prof, err = buildCurveProfileFromRanges(config.TemperatureRanges)
		if err != nil {
			log.Printf("WARN: curve mode requested but invalid curve profile: %v. Falling back to step mode.", err)
			useCurve = false
		} else {
			log.Printf("INFO: Curve mode enabled: floor(<%d°C)=AUTO, setpoints=%v (floor hyst=%d°C)",
				prof.floorEndTemp, prof.points, prof.floorHyst)
		}
	}

	// Track whether each GPU is currently in AUTO (below floor) or MANUAL (above floor).
	inAuto := make([]bool, count)
	for i := 0; i < count; i++ {
		inAuto[i] = prevTemps[i] < prof.floorEndTemp
	}

	// For manual-mode hysteresis on the curve target
	lastFanChangeTemp := make([]int, count)
	copy(lastFanChangeTemp, prevTemps)

	ticker := time.NewTicker(time.Duration(config.TimeToUpdate) * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		for i := 0; i < count; i++ {
			if fanCounts[i] == 0 {
				continue
			}

			device, ret := nvml.DeviceGetHandleByIndex(i)
			if ret != nvml.SUCCESS {
				log.Printf("ERROR: Unable to get handle for device %d during update: %v. Skipping cycle for this device.", i, nvml.ErrorString(ret))
				continue
			}

			temp, ret := nvml.DeviceGetTemperature(device, nvml.TEMPERATURE_GPU)
			if ret != nvml.SUCCESS {
				log.Printf("ERROR: Unable to get temperature for device %d: %v. Skipping cycle for this device.", i, nvml.ErrorString(ret))
				continue
			}
			tempInt := int(temp)

			if useCurve {
				// --- Decide AUTO vs MANUAL using a deadband around floorEndTemp ---
				// If we're in AUTO, only leave AUTO when temp >= floorEndTemp + floorHyst
				// If we're in MANUAL, only enter AUTO when temp <= floorEndTemp - floorHyst
				if inAuto[i] {
					if tempInt >= prof.floorEndTemp+prof.floorHyst {
						inAuto[i] = false
						log.Printf("INFO: GPU %d crossing above floor: switching to MANUAL control (temp=%d°C)", i, tempInt)
					}
				} else {
					if tempInt <= prof.floorEndTemp-prof.floorHyst {
						inAuto[i] = true
						log.Printf("INFO: GPU %d crossing below floor: switching to AUTO control (temp=%d°C)", i, tempInt)
					}
				}

				// --- Apply policy ---
				if inAuto[i] {
					// Below floor => AUTO policy; do not set speed.
					for fanIdx := 0; fanIdx < fanCounts[i]; fanIdx++ {
						ret = nvml.DeviceSetFanControlPolicy(device, fanIdx, nvml.FAN_POLICY_TEMPERATURE_CONTINOUS_SW)
						if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED {
							log.Printf("ERROR: Unable to set AUTO fan policy for GPU %d Fan %d: %v", i, fanIdx, nvml.ErrorString(ret))
							continue
						} else if ret == nvml.ERROR_NOT_SUPPORTED {
							log.Printf("WARN: AUTO fan policy not supported for GPU %d Fan %d.", i, fanIdx)
							continue
						}
					}

					// In AUTO, we should not treat any previous manual temp as the hysteresis reference.
					// Reset the "last change" reference so when we re-enter MANUAL we don't block updates.
					lastFanChangeTemp[i] = tempInt
					prevTemps[i] = tempInt
					continue
				}

				// Above floor => MANUAL policy + curve target.
				anyFanUpdated := false
				for fanIdx := 0; fanIdx < fanCounts[i]; fanIdx++ {
					prevSpeed := prevFanSpeeds[i][fanIdx]
					newFanSpeed, hyst := curveSpeedForTempWithProfile(tempInt, prof)

					if newFanSpeed == prevSpeed {
						continue
					}

					// Curve hysteresis: compare to last successful change temperature.
					if abs(tempInt-lastFanChangeTemp[i]) < hyst {
						continue
					}

					ret = nvml.DeviceSetFanControlPolicy(device, fanIdx, nvml.FAN_POLICY_MANUAL)
					if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED {
						log.Printf("ERROR: Unable to set MANUAL fan policy for GPU %d Fan %d: %v", i, fanIdx, nvml.ErrorString(ret))
						continue
					} else if ret == nvml.ERROR_NOT_SUPPORTED {
						log.Printf("WARN: MANUAL fan policy not supported for GPU %d Fan %d.", i, fanIdx)
						continue
					}

					ret = nvml.DeviceSetFanSpeed_v2(device, fanIdx, newFanSpeed)
					if ret != nvml.SUCCESS {
						log.Printf("ERROR: Unable to set fan speed for GPU %d Fan %d to %d%%: %v", i, fanIdx, newFanSpeed, nvml.ErrorString(ret))
						continue
					}

					log.Printf("INFO: Updated GPU %d Fan %d (curve): Temp=%d°C, PrevSpeed=%d%%, NewSpeed=%d%%, Hyst=%d°C",
						i, fanIdx, tempInt, prevSpeed, newFanSpeed, hyst)

					prevFanSpeeds[i][fanIdx] = newFanSpeed
					anyFanUpdated = true
				}

				if anyFanUpdated {
					lastFanChangeTemp[i] = tempInt
				}
				prevTemps[i] = tempInt
				continue
			}

			// --- Original step mode unchanged ---
			for fanIdx := 0; fanIdx < fanCounts[i]; fanIdx++ {
				prevSpeed := prevFanSpeeds[i][fanIdx]
				newFanSpeed := getFanSpeedForTemperature(tempInt, prevTemps[i], prevSpeed, config.TemperatureRanges)
				if newFanSpeed == prevSpeed {
					continue
				}

				ret = nvml.DeviceSetFanControlPolicy(device, fanIdx, nvml.FAN_POLICY_MANUAL)
				if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED {
					log.Printf("ERROR: Unable to set manual fan control policy for GPU %d Fan %d: %v", i, fanIdx, nvml.ErrorString(ret))
					continue
				} else if ret == nvml.ERROR_NOT_SUPPORTED {
					log.Printf("WARN: Manual fan control policy not supported for GPU %d Fan %d. Cannot set speed.", i, fanIdx)
					continue
				}

				ret = nvml.DeviceSetFanSpeed_v2(device, fanIdx, newFanSpeed)
				if ret != nvml.SUCCESS {
					log.Printf("ERROR: Unable to set fan speed for GPU %d Fan %d to %d%%: %v", i, fanIdx, newFanSpeed, nvml.ErrorString(ret))
					continue
				}

				log.Printf("INFO: Updated GPU %d Fan %d: Temp=%d°C, PrevSpeed=%d%%, NewSpeed=%d%%",
					i, fanIdx, tempInt, prevSpeed, newFanSpeed)

				prevFanSpeeds[i][fanIdx] = newFanSpeed
			}
			prevTemps[i] = tempInt
		}
	}
}


// ---------- CLI plumbing (quiet by default) ----------

func printUsage() {
	fmt.Fprintf(os.Stderr, `Usage:
  nvidia_fan_control daemon   [-config PATH] [-log PATH] [-curve]
  nvidia_fan_control status   [-gpu N] [-v]
  nvidia_fan_control set      [-gpu N] [-fans "0,1"] -speed PERCENT [-v]
  nvidia_fan_control auto     [-gpu N] [-fans "0,1"] [-v]

daemon mode is EXACTLY the original behavior by default:
  - reads config.json from current directory
  - logs to /var/log/nvidia_fan_control.log

Curve mode (daemon only):
  - uses the LOWEST-min range as the floor region:
      temps < floor.max_temperature => floor.fan_speed
  - uses subsequent ranges as setpoints at min_temperature
  - interpolates only between setpoints (smooth transition), with floor+ceiling clamps
`)
}

func parseFanList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("fans list is empty")
	}
	parts := strings.Split(s, ",")
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid fan index %q: %w", p, err)
		}
		if n < 0 {
			return nil, fmt.Errorf("invalid fan index %d: must be >= 0", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid fan indices parsed from %q", s)
	}
	return out, nil
}

func deviceHandleByIndex(idx int) (nvml.Device, error) {
	dev, ret := nvml.DeviceGetHandleByIndex(idx)
	if ret != nvml.SUCCESS {
		return dev, fmt.Errorf("unable to get handle for device %d: %v", idx, nvml.ErrorString(ret))
	}
	return dev, nil
}

func getFanSpeedPercent(device nvml.Device, fanIdx int) (int, error) {
	speed, ret := nvml.DeviceGetFanSpeed_v2(device, fanIdx)
	if ret == nvml.SUCCESS {
		return int(speed), nil
	}

	if fanIdx == 0 {
		speedLegacy, retLegacy := nvml.DeviceGetFanSpeed(device)
		if retLegacy == nvml.SUCCESS {
			return int(speedLegacy), nil
		}
		return 0, fmt.Errorf("fan speed v2 failed (%v) and legacy failed (%v)", nvml.ErrorString(ret), nvml.ErrorString(retLegacy))
	}

	return 0, fmt.Errorf("fan speed v2 not available for fan %d: %v", fanIdx, nvml.ErrorString(ret))
}

func configureCLILogging(verbose bool) {
	if verbose {
		log.SetOutput(os.Stderr)
		log.SetFlags(log.LstdFlags)
	} else {
		log.SetOutput(io.Discard)
		log.SetFlags(0)
	}
}

func cmdStatus(gpuIdx int, verbose bool) int {
	configureCLILogging(verbose)

	cleanup, err := initializeNVML()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		fmt.Fprintf(os.Stderr, "unable to get NVIDIA device count: %v\n", nvml.ErrorString(ret))
		return 1
	}
	if gpuIdx < 0 || gpuIdx >= count {
		fmt.Fprintf(os.Stderr, "invalid -gpu %d (found %d device(s), valid range: 0..%d)\n", gpuIdx, count, count-1)
		return 1
	}

	dev, err := deviceHandleByIndex(gpuIdx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	temp, ret := nvml.DeviceGetTemperature(dev, nvml.TEMPERATURE_GPU)
	if ret != nvml.SUCCESS {
		fmt.Fprintf(os.Stderr, "unable to get temperature for device %d: %v\n", gpuIdx, nvml.ErrorString(ret))
		return 1
	}

	numFans, ret := nvml.DeviceGetNumFans(dev)
	if ret != nvml.SUCCESS {
		fmt.Printf("GPU %d: Temp=%d°C, Fans=unknown (DeviceGetNumFans: %v)\n", gpuIdx, int(temp), nvml.ErrorString(ret))
		return 0
	}

	fmt.Printf("GPU %d: Temp=%d°C, Fans=%d\n", gpuIdx, int(temp), numFans)
	for fanIdx := 0; fanIdx < numFans; fanIdx++ {
		speedPct, err := getFanSpeedPercent(dev, fanIdx)
		if err != nil {
			fmt.Printf("  Fan %d: speed=unknown (%v)\n", fanIdx, err)
			continue
		}
		fmt.Printf("  Fan %d: speed=%d%%\n", fanIdx, speedPct)
	}
	return 0
}

func cmdSet(gpuIdx int, fans []int, speed int, verbose bool) int {
	configureCLILogging(verbose)

	if speed < 0 || speed > 100 {
		fmt.Fprintf(os.Stderr, "-speed must be 0..100 (got %d)\n", speed)
		return 1
	}

	cleanup, err := initializeNVML()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		fmt.Fprintf(os.Stderr, "unable to get NVIDIA device count: %v\n", nvml.ErrorString(ret))
		return 1
	}
	if gpuIdx < 0 || gpuIdx >= count {
		fmt.Fprintf(os.Stderr, "invalid -gpu %d (found %d device(s), valid range: 0..%d)\n", gpuIdx, count, count-1)
		return 1
	}

	dev, err := deviceHandleByIndex(gpuIdx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	numFans, ret := nvml.DeviceGetNumFans(dev)
	if ret != nvml.SUCCESS {
		fmt.Fprintf(os.Stderr, "unable to get fan count for device %d: %v\n", gpuIdx, nvml.ErrorString(ret))
		return 1
	}

	for _, fanIdx := range fans {
		if fanIdx < 0 || fanIdx >= numFans {
			fmt.Fprintf(os.Stderr, "invalid fan index %d for GPU %d (device reports %d fan(s))\n", fanIdx, gpuIdx, numFans)
			return 1
		}
	}

	for _, fanIdx := range fans {
		ret = nvml.DeviceSetFanControlPolicy(dev, fanIdx, nvml.FAN_POLICY_MANUAL)
		if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED {
			fmt.Fprintf(os.Stderr, "unable to set manual fan policy for GPU %d Fan %d: %v\n", gpuIdx, fanIdx, nvml.ErrorString(ret))
			return 1
		}
		if ret == nvml.ERROR_NOT_SUPPORTED {
			fmt.Fprintf(os.Stderr, "manual fan policy not supported for GPU %d Fan %d\n", gpuIdx, fanIdx)
			return 1
		}

		ret = nvml.DeviceSetFanSpeed_v2(dev, fanIdx, speed)
		if ret != nvml.SUCCESS {
			fmt.Fprintf(os.Stderr, "unable to set fan speed for GPU %d Fan %d to %d%%: %v\n", gpuIdx, fanIdx, speed, nvml.ErrorString(ret))
			return 1
		}
	}

	return 0
}

func cmdAuto(gpuIdx int, fans []int, verbose bool) int {
	configureCLILogging(verbose)

	cleanup, err := initializeNVML()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer cleanup()

	count, ret := nvml.DeviceGetCount()
	if ret != nvml.SUCCESS {
		fmt.Fprintf(os.Stderr, "unable to get NVIDIA device count: %v\n", nvml.ErrorString(ret))
		return 1
	}
	if gpuIdx < 0 || gpuIdx >= count {
		fmt.Fprintf(os.Stderr, "invalid -gpu %d (found %d device(s), valid range: 0..%d)\n", gpuIdx, count, count-1)
		return 1
	}

	dev, err := deviceHandleByIndex(gpuIdx)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	numFans, ret := nvml.DeviceGetNumFans(dev)
	if ret != nvml.SUCCESS {
		fmt.Fprintf(os.Stderr, "unable to get fan count for device %d: %v\n", gpuIdx, nvml.ErrorString(ret))
		return 1
	}

	for _, fanIdx := range fans {
		if fanIdx < 0 || fanIdx >= numFans {
			fmt.Fprintf(os.Stderr, "invalid fan index %d for GPU %d (device reports %d fan(s))\n", fanIdx, gpuIdx, numFans)
			return 1
		}
	}

	for _, fanIdx := range fans {
		ret = nvml.DeviceSetFanControlPolicy(dev, fanIdx, nvml.FAN_POLICY_TEMPERATURE_CONTINOUS_SW)
		if ret != nvml.SUCCESS && ret != nvml.ERROR_NOT_SUPPORTED {
			fmt.Fprintf(os.Stderr, "unable to set temperature fan policy for GPU %d Fan %d: %v\n", gpuIdx, fanIdx, nvml.ErrorString(ret))
			return 1
		}
		if ret == nvml.ERROR_NOT_SUPPORTED {
			fmt.Fprintf(os.Stderr, "temperature fan policy not supported for GPU %d Fan %d\n", gpuIdx, fanIdx)
			return 1
		}
	}

	return 0
}

func cmdDaemon(configPath, logPath string, curveOverride bool) int {
	logFile, err := setupLogging(logPath)
	if err != nil {
		log.Printf("FATAL: %v", err)
		return 1
	}
	defer logFile.Close()

	config, err := loadConfiguration(configPath)
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}

	if curveOverride {
		config.Curve = true
	}

	nvmlCleanup, err := initializeNVML()
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}
	defer nvmlCleanup()

	count, fanCounts, prevTemps, prevFanSpeeds, err := initializeDevices()
	if err != nil {
		log.Fatalf("FATAL: %v", err)
	}

	hasControllableFans := false
	for _, fc := range fanCounts {
		if fc > 0 {
			hasControllableFans = true
			break
		}
	}

	if !hasControllableFans {
		log.Println("INFO: No devices with controllable fans were found or initialized. Exiting.")
		return 0
	}

	runMonitoringLoop(config, count, fanCounts, prevTemps, prevFanSpeeds)
	log.Println("INFO: Monitoring loop finished unexpectedly.")
	return 0
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "daemon":
		fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
		configPath := fs.String("config", "config.json", "Path to config.json (default preserves original behavior)")
		logPath := fs.String("log", "/var/log/nvidia_fan_control.log", "Log file path (default preserves original behavior)")
		curve := fs.Bool("curve", false, "Enable curve mode (overrides config)")
		fs.SetOutput(os.Stderr)
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		os.Exit(cmdDaemon(*configPath, *logPath, *curve))

	case "status":
		fs := flag.NewFlagSet("status", flag.ContinueOnError)
		gpuIdx := fs.Int("gpu", 0, "GPU index (default 0)")
		verbose := fs.Bool("v", false, "Verbose (print NVML init/shutdown logs)")
		fs.SetOutput(os.Stderr)
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		os.Exit(cmdStatus(*gpuIdx, *verbose))

	case "set":
		fs := flag.NewFlagSet("set", flag.ContinueOnError)
		gpuIdx := fs.Int("gpu", 0, "GPU index (default 0)")
		fansStr := fs.String("fans", "0", "Comma-separated fan indices (default 0)")
		speed := fs.Int("speed", -1, "Fan speed percent 0..100 (required)")
		verbose := fs.Bool("v", false, "Verbose (print NVML init/shutdown logs)")
		fs.SetOutput(os.Stderr)
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		if *speed < 0 {
			fmt.Fprintln(os.Stderr, "set: -speed is required")
			os.Exit(2)
		}
		fans, err := parseFanList(*fansStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "set:", err)
			os.Exit(2)
		}
		os.Exit(cmdSet(*gpuIdx, fans, *speed, *verbose))

	case "auto":
		fs := flag.NewFlagSet("auto", flag.ContinueOnError)
		gpuIdx := fs.Int("gpu", 0, "GPU index (default 0)")
		fansStr := fs.String("fans", "0", "Comma-separated fan indices (default 0)")
		verbose := fs.Bool("v", false, "Verbose (print NVML init/shutdown logs)")
		fs.SetOutput(os.Stderr)
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		fans, err := parseFanList(*fansStr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "auto:", err)
			os.Exit(2)
		}
		os.Exit(cmdAuto(*gpuIdx, fans, *verbose))

	default:
		printUsage()
		os.Exit(2)
	}
}
