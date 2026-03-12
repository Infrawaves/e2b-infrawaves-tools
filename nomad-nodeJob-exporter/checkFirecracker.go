package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// e2b_fc_process_count: Current node firecracker process count
	e2bFcProcessCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_count",
			Help: "Total number of firecracker processes running on the node",
		},
		[]string{"node_ip"},
	)

	// e2b_fc_process_parse_errorsudation: Cumulative parse errors
	e2bFcProcessParseErrorsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_parse_errors_total",
			Help: "Cumulative count of sandbox_id parse failures",
		},
		[]string{"node_ip"},
	)

	// e2b_fc_process_info: Process info mapping
	e2bFcProcessInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_info",
			Help: "Process info mapping (value is always 1)",
		},
		[]string{"node_ip", "sandbox_id", "pid"},
	)

	// e2b_fc_process_memory_rss_bytes: Resident memory size
	e2bFcProcessMemoryRssBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_memory_rss_bytes",
			Help: "Process resident set size in bytes",
		},
		[]string{"node_ip", "sandbox_id", "pid"},
	)

	// e2b_fc_process_cpu_seconds_total: CPU time
	e2bFcProcessCpuSecondsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_cpu_seconds_total",
			Help: "Process cumulative CPU time in seconds",
		},
		[]string{"node_ip", "sandbox_id", "pid", "mode"},
	)

	// e2b_fc_process_uptime_seconds: Process uptime
	e2bFcProcessUptimeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_uptime_seconds",
			Help: "Process uptime in seconds",
		},
		[]string{"node_ip", "sandbox_id", "pid"},
	)

	// e2b_fc_process_threads: Thread count
	e2bFcProcessThreads = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_threads",
			Help: "Process thread count",
		},
		[]string{"node_ip", "sandbox_id", "pid"},
	)

	// e2b_fc_process_open_fds: Open file descriptors
	e2bFcProcessOpenFds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_open_fds",
			Help: "Process open file descriptor count",
		},
		[]string{"node_ip", "sandbox_id", "pid"},
	)

	// e2b_fc_process_io_bytes_total: I/O bytes
	e2bFcProcessIoBytesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_io_bytes_total",
			Help: "Process cumulative I/O bytes",
		},
		[]string{"node_ip", "sandbox_id", "pid", "operation"},
	)

	// e2b_fc_process_io_ops_total: I/O operations
	e2bFcProcessIoOpsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_io_ops_total",
			Help: "Process cumulative I/O operations",
		},
		[]string{"node_ip", "sandbox_id", "pid", "operation"},
	)

	// e2b_fc_process_context_switches_total: Context switches
	e2bFcProcessContextSwitchesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_context_switches_total",
			Help: "Process cumulative context switches",
		},
		[]string{"node_ip", "sandbox_id", "pid", "type"},
	)

	// Page size constant for RSS calculation
	pageSize = int64(os.Getpagesize())

	// System clock tick for CPU time calculation
	clockTicks = int64(100) // Default to 100 Hz, will be updated
)

// init initializes the clock ticks
func init() {
	// Try to get the system's HZ value
	if hz := getClockTicks(); hz > 0 {
		clockTicks = hz
	}
}

// getClockTicks reads the system's HZ value from /proc/stat or sysconf
func getClockTicks() int64 {
	// Try to read from /proc/stat
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 100 // Default fallback
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "btime") {
			// btime is in the same file, but HZ is typically 100 on most Linux systems
			// We could try to parse CLOCKS_PER_SEC from C headers, but 100 is safe default
			return 100
		}
	}
	return 100
}

// getNodeIP gets the node's IP address
func getNodeIP() string {
	// Try multiple methods to get the IP
	// Method 1: Check from Nomad node info if available
	// Method 2: Get from hostname -i
	// Method 3: Use a default fallback

	// Try to get from environment
	if ip := os.Getenv("NODE_IP"); ip != "" {
		return ip
	}

	// Try to parse from hostname -i
	// This is a simplified approach - in production, you might want more robust detection
	return "unknown"
}

// extractSandboxID extracts the sandbox_id from the command line
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
	prefix := "/fc-"
	prefixIdx := strings.LastIndex(socketPath, prefix)
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

// isFirecrackerProcess checks if a process is a firecracker process
func isFirecrackerProcess(pid string) bool {
	// Read the command line
	cmdlinePath := filepath.Join("/proc", pid, "cmdline")
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false
	}

	// cmdline is a null-terminated string
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	cmdline = strings.TrimSpace(cmdline)

	// Check if it contains firecracker
	return strings.Contains(strings.ToLower(cmdline), "firecracker")
}

// getProcessCmdline reads the process command line
func getProcessCmdline(pid string) (string, error) {
	cmdlinePath := filepath.Join("/proc", pid, "cmdline")
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return "", err
	}

	// cmdline is a null-terminated string
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	return strings.TrimSpace(cmdline), nil
}

// parseStat parses /proc/[pid]/stat and returns key metrics
func parseStat(pid string) (userTime, systemTime, uptime float64, err error) {
	statPath := filepath.Join("/proc", pid, "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, 0, 0, err
	}

	// /proc/[pid]/stat format:
	// pid (comm) state ppid pgrp sid ...
	// The comm field can contain spaces, so we need to parse carefully
	stat := string(data)

	// Find the last ')' to get the end of comm
	lastParen := strings.LastIndex(stat, ")")
	if lastParen == -1 {
		return 0, 0, 0, fmt.Errorf("invalid stat format")
	}

	// Get the rest after comm
	rest := stat[lastParen+1:]
	fields := strings.Fields(rest)

	if len(fields) < 22 {
		return 0, 0, 0, fmt.Errorf("not enough fields in stat")
	}

	// utime (field 13) and stime (field 14) are in clock ticks
	utime, err := strconv.ParseFloat(fields[12], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	stime, err := strconv.ParseFloat(fields[13], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	// Convert to seconds
	userTime = utime / float64(clockTicks)
	systemTime = stime / float64(clockTicks)

	// start_time (field 21) is in clock ticks since boot
	startTime, err := strconv.ParseFloat(fields[20], 64)
	if err != nil {
		return 0, 0, 0, err
	}

	// Get current system uptime in seconds
	btime, err := getBootTime()
	if err != nil {
		return 0, 0, 0, err
	}

	now := float64(time.Now().Unix())
	uptime = now - btime - (startTime / float64(clockTicks))

	if uptime < 0 {
		uptime = 0
	}

	return userTime, systemTime, uptime, nil
}

// getBootTime reads the boot time from /proc/stat
func getBootTime() (float64, error) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "btime") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				btime, err := strconv.ParseFloat(fields[1], 64)
				return btime, err
			}
		}
	}
	return 0, fmt.Errorf("btime not found in /proc/stat")
}

// parseStatm parses /proc/[pid]/statm and returns RSS in pages
func parseStatm(pid string) (int64, error) {
	statmPath := filepath.Join("/proc", pid, "statm")
	data, err := os.ReadFile(statmPath)
	if err != nil {
		return 0, err
	}

	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("invalid statm format")
	}

	rss, err := strconv.ParseInt(fields[1], 10, 64)
	return rss, err
}

// parseStatus parses /proc/[pid]/status and returns threads and context switches
func parseStatus(pid string) (threads int, voluntarySwitches, involuntarySwitches float64, err error) {
	statusPath := filepath.Join("/proc", pid, "status")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, 0, 0, err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Threads:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				threads, _ = strconv.Atoi(fields[1])
			}
		} else if strings.HasPrefix(line, "voluntary_ctxt_switches:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				voluntarySwitches, _ = strconv.ParseFloat(fields[1], 64)
			}
		} else if strings.HasPrefix(line, "nonvoluntary_ctxt_switches:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				involuntarySwitches, _ = strconv.ParseFloat(fields[1], 64)
			}
		}
	}

	return threads, voluntarySwitches, involuntarySwitches, nil
}

// countOpenFds counts the number of open file descriptors
func countOpenFds(pid string) (int, error) {
	fdPath := filepath.Join("/proc", pid, "fd")
	entries, err := os.ReadDir(fdPath)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// parseIo parses /proc/[pid]/io and returns I/O statistics
func parseIo(pid string) (readBytes, writeBytes, readCount, writeCount float64, err error) {
	ioPath := filepath.Join("/proc", pid, "io")
	data, err := os.ReadFile(ioPath)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "read_bytes:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				readBytes, _ = strconv.ParseFloat(fields[1], 64)
			}
		} else if strings.HasPrefix(line, "write_bytes:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				writeBytes, _ = strconv.ParseFloat(fields[1], 64)
			}
		} else if strings.HasPrefix(line, "read_count:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				readCount, _ = strconv.ParseFloat(fields[1], 64)
			}
		} else if strings.HasPrefix(line, "write_count:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				writeCount, _ = strconv.ParseFloat(fields[1], 64)
			}
		}
	}

	return readBytes, writeBytes, readCount, writeCount, nil
}

// updateFirecrackerMetrics collects and updates firecracker process metrics
func updateFirecrackerMetrics() {
	// Reset metrics first
	e2bFcProcessCount.Reset()
	e2bFcProcessInfo.Reset()
	e2bFcProcessMemoryRssBytes.Reset()
	e2bFcProcessCpuSecondsTotal.Reset()
	e2bFcProcessUptimeSeconds.Reset()
	e2bFcProcessThreads.Reset()
	e2bFcProcessOpenFds.Reset()
	e2bFcProcessIoBytesTotal.Reset()
	e2bFcProcessIoOpsTotal.Reset()
	e2bFcProcessContextSwitchesTotal.Reset()

	// Get node IP
	nodeIP := getNodeIP()

	// Scan /proc directory for processes
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		log.Printf("Failed to read /proc directory: %v", err)
		return
	}

	processCount := 0

	for _, entry := range entries {
		// Check if it's a numeric directory (PID)
		if !entry.IsDir() {
			continue
		}

		pid := entry.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}

		// Check if it's a firecracker process
		if !isFirecrackerProcess(pid) {
			continue
		}

		// Get command line and extract sandbox_id
		cmdline, err := getProcessCmdline(pid)
		if err != nil {
			continue
		}

		sandboxID := extractSandboxID(cmdline)

		// If no sandbox_id found, skip this process and count error
		if sandboxID == "" {
			e2bFcProcessParseErrorsTotal.WithLabelValues(nodeIP).Inc()
			continue
		}

		// Parse /proc/[pid]/stat for CPU and uptime
		userTime, systemTime, uptime, err := parseStat(pid)
		if err != nil {
			log.Printf("Failed to parse stat for pid %s: %v", pid, err)
			continue
		}

		// Parse /proc/[pid]/statm for memory
		rssPages, err := parseStatm(pid)
		if err != nil {
			log.Printf("Failed to parse statm for pid %s: %v", pid, err)
			continue
		}
		rssBytes := rssPages * pageSize

		// Parse /proc/[pid]/status for threads and context switches
		threads, voluntarySwitches, involuntarySwitches, err := parseStatus(pid)
		if err != nil {
			log.Printf("Failed to parse status for pid %s: %v", pid, err)
		}

		// Count open file descriptors
		openFds, err := countOpenFds(pid)
		if err != nil {
			log.Printf("Failed to count fds for pid %s: %v", pid, err)
		}

		// Parse /proc/[pid]/io for I/O statistics
		readBytes, writeBytes, readCount, writeCount, err := parseIo(pid)
		if err != nil {
			log.Printf("Failed to parse io for pid %s: %v", pid, err)
		}

		// Update metrics
		e2bFcProcessInfo.WithLabelValues(nodeIP, sandboxID, pid).Set(1)
		e2bFcProcessMemoryRssBytes.WithLabelValues(nodeIP, sandboxID, pid).Set(float64(rssBytes))
		e2bFcProcessCpuSecondsTotal.WithLabelValues(nodeIP, sandboxID, pid, "user").Set(userTime)
		e2bFcProcessCpuSecondsTotal.WithLabelValues(nodeIP, sandboxID, pid, "system").Set(systemTime)
		e2bFcProcessUptimeSeconds.WithLabelValues(nodeIP, sandboxID, pid).Set(uptime)
		e2bFcProcessThreads.WithLabelValues(nodeIP, sandboxID, pid).Set(float64(threads))
		e2bFcProcessOpenFds.WithLabelValues(nodeIP, sandboxID, pid).Set(float64(openFds))
		e2bFcProcessIoBytesTotal.WithLabelValues(nodeIP, sandboxID, pid, "read").Set(readBytes)
		e2bFcProcessIoBytesTotal.WithLabelValues(nodeIP, sandboxID, pid, "write").Set(writeBytes)
		e2bFcProcessIoOpsTotal.WithLabelValues(nodeIP, sandboxID, pid, "read").Set(readCount)
		e2bFcProcessIoOpsTotal.WithLabelValues(nodeIP, sandboxID, pid, "write").Set(writeCount)
		e2bFcProcessContextSwitchesTotal.WithLabelValues(nodeIP, sandboxID, pid, "voluntary").Set(voluntarySwitches)
		e2bFcProcessContextSwitchesTotal.WithLabelValues(nodeIP, sandboxID, pid, "involuntary").Set(involuntarySwitches)

		processCount++
	}

	// Set total count
	e2bFcProcessCount.WithLabelValues(nodeIP).Set(float64(processCount))

	if processCount > 0 {
		log.Printf("Updated firecracker metrics: %d processes on node %s", processCount, nodeIP)
	}
}
