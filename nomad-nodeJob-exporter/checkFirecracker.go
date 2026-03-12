package main

import (
	"bufio"
	"bytes"
	"fmt"
	"log"
	"os/exec"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	firecrackerProcessTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "firecracker_process_total",
			Help: "Total number of firecracker processes running on the node",
		},
		[]string{}, // no labels for total count
	)

	firecrackerUptimeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "firecracker_uptime_seconds",
			Help: "Uptime of each firecracker process in seconds",
		},
		[]string{"sandbox_id"}, // sandbox_id as the only label
	)
)

// parsePsTime parses the time string from ps aux to seconds
// Formats can be: "1:23.45" (MM:SS), "12:34:56" (HH:MM:SS), "123-12:34" (DD-HH:MM)
func parsePsTime(timeStr string) (float64, error) {
	timeStr = strings.TrimSpace(timeStr)

	// Check if it's in the format "DD-HH:MM" (days)
	if strings.Contains(timeStr, "-") {
		parts := strings.Split(timeStr, "-")
		if len(parts) != 2 {
			return 0, fmt.Errorf("invalid time format: %s", timeStr)
		}
		days, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		hmSeconds, err := parseHMToSeconds(parts[1])
		if err != nil {
			return 0, err
		}
		return float64(days*86400 + hmSeconds), nil
	}

	// Check if it's in the format "HH:MM:SS"
	if strings.Count(timeStr, ":") == 2 {
		parts := strings.Split(timeStr, ":")
		if len(parts) != 3 {
			return 0, fmt.Errorf("invalid time format: %s", timeStr)
		}
		hours, err := strconv.Atoi(parts[0])
		if err != nil {
			return 0, err
		}
		minutes, err := strconv.Atoi(parts[1])
		if err != nil {
			return 0, err
		}
		seconds, err := strconv.Atoi(parts[2])
		if err != nil {
			return 0, err
		}
		return float64(hours*3600 + minutes*60 + seconds), nil
	}

		// Check if it's in the format "MM:SS" or "MM:SS.mmm"
	if strings.Count(timeStr, ":") == 1 {
		seconds, err := parseHMToSeconds(timeStr)
		if err != nil {
			return 0, err
		}
		return float64(seconds), nil
	}

	// Try to parse as integer seconds (etime format on some systems)
	seconds, err := strconv.Atoi(timeStr)
	if err == nil {
		return float64(seconds), nil
	}

	return 0, fmt.Errorf("unsupported time format: %s", timeStr)
}

// parseHMToSeconds parses "MM:SS" or "MM:SS.mmm" to seconds
func parseHMToSeconds(hmStr string) (int, error) {
	parts := strings.Split(hmStr, ":")
	if len(parts) != 2 {
		return 0, fmt.Errorf("invalid HM format: %s", hmStr)
	}
	minutes, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, err
	}
	secondsStr := strings.Split(parts[1], ".")[0] // remove fractional seconds if present
	seconds, err := strconv.Atoi(secondsStr)
	if err != nil {
		return 0, err
	}
	return minutes*60 + seconds, nil
}

// extractSandboxID extracts the sandbox_id from the command line
// It looks for --api-sock followed by a socket path in format: /tmp/fc-{sandboxID}-{randomID}.sock
func extractSandboxID(commandLine string) string {
	// Find --api-sock in the command line
	apiSockIdx := strings.Index(commandLine, "--api-sock")
	if apiSockIdx == -1 {
		return ""
	}

	// Get the rest of the command after --api-sock
	rest := commandLine[apiSockIdx+len("--api-sock"):]
	rest = strings.TrimSpace(rest)

	if rest == "" {
		return ""
	}

	// The socket path should be the next argument
	// Split by spaces and get the first part
	socketPath := strings.Split(rest, " ")[0]

	// Extract sandbox_id from socket path
	// Pattern: /fc-{sandboxID}-{randomID}.sock
	// Match: /fc- followed by non-dash characters, then dash, then non-slash characters, then .sock
	prefix := "/fc-"
	prefixIdx := strings.LastIndex(socketPath, prefix) // use LastIndex to handle paths like /fc-versions/...
	if prefixIdx == -1 {
		return ""
	}

	afterPrefix := socketPath[prefixIdx+len(prefix):]
	dashIdx := strings.Index(afterPrefix, "-")
	if dashIdx == -1 {
		return ""
	}

	sandboxID := afterPrefix[:dashIdx]

	// Validate sandbox_id is not empty
	if sandboxID == "" {
		return ""
	}

	return sandboxID
}

// updateFirecrackerMetrics collects and updates firecracker process metrics
func updateFirecrackerMetrics() {
	// Reset metrics first
	firecrackerProcessTotal.Reset()
	firecrackerUptimeSeconds.Reset()

	// Run ps aux to get firecracker processes
	cmd := exec.Command("ps", "aux")
	output, err := cmd.Output()
	if err != nil {
		return
	}

	scanner := bufio.NewScanner(bytes.NewReader(output))
	lineNum := 0
	processCount := 0

	for scanner.Scan() {
		line := scanner.Text()
		lineNum++

		// Skip header line
		if lineNum == 1 {
			continue
		}

		// Check if this line contains firecracker
		if !strings.Contains(strings.ToLower(line), "firecracker") {
			continue
		}

		// Skip the grep command itself if present
		if strings.Contains(line, "grep") {
			continue
		}

		// Parse the line
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}

		pid := fields[1]
		// ELAPSED time is typically at position 9 (0-indexed) in ps aux
		elapsedTime := fields[9]

		// Parse uptime
		uptimeSeconds, err := parsePsTime(elapsedTime)
		if err != nil {
			continue // skip if we can't parse the time
		}

		// Extract command line (from field 10 onwards)
		commandLine := strings.Join(fields[10:], " ")

		// Extract sandbox_id from command line
		sandboxID := extractSandboxID(commandLine)

		// If no sandbox_id found, use pid as fallback
		if sandboxID == "" {
			sandboxID = pid
		}

		// Set uptime metric
		firecrackerUptimeSeconds.WithLabelValues(sandboxID).Set(uptimeSeconds)

		processCount++
	}

	// Set total count
	firecrackerProcessTotal.WithLabelValues().Set(float64(processCount))

	if processCount > 0 {
		log.Printf("Updated firecracker metrics: %d processes", processCount)
	}
}
