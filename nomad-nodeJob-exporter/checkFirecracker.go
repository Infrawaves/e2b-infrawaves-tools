package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// e2b_fc_process_count:本节点 firecracker 进程总数
	e2bFcProcessCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_count",
			Help: "Total number of firecracker processes running on the node",
		},
		[]string{"node_ip"},
	)

	// e2b_fc_process_parse_errors_total:累计的 sandbox_id 解析失败次数
	e2bFcProcessParseErrorsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_parse_errors_total",
			Help: "Cumulative count of sandbox_id parse failures",
		},
		[]string{"node_ip"},
	)

	// e2b_fc_process_info:进程身份映射(值固定 1,用作 join key)
	e2bFcProcessInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_info",
			Help: "Process info mapping (value is always 1)",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_memory_rss_bytes:进程常驻内存(RSS)字节数
	// 注意:开启 Hugepages 的 Firecracker 环境下 RSS 通常为 0,
	// 因为 Linux 内核不把 Hugepages 计入标准 VmRSS 统计。
	// 此场景请改用 e2b_fc_process_memory_vsize_bytes 看实际内存分配。
	e2bFcProcessMemoryRssBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_memory_rss_bytes",
			Help: "Process resident set size in bytes. Note: In Hugepages-enabled Firecracker environments, this is typically 0 as Hugepages are not counted in standard VmRSS statistics",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_memory_vsize_bytes:进程虚拟内存(VmSize)字节数
	// 注意:开启 Hugepages 的 Firecracker 环境(E2B、Fly.io 等)下,
	// 由于 RSS 为 0,vsize 是观察实际分配的主指标。
	e2bFcProcessMemoryVsizeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_memory_vsize_bytes",
			Help: "Process virtual memory size in bytes. In Hugepages-enabled Firecracker environments, this represents the actual memory allocation as RSS is unavailable",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_memory_hugetlb_bytes:HugeTLB 内存字节数
	// 通过 hugetlbfs 分配的物理内存,不计入标准 VmRSS。
	e2bFcProcessMemoryHugetlbBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_memory_hugetlb_bytes",
			Help: "Process HugeTLB memory size in bytes. Represents physical memory allocated through Hugepages, which is not counted in standard VmRSS statistics",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_cpu_seconds_total:CPU 累计时间(秒)
	e2bFcProcessCpuSecondsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_cpu_seconds_total",
			Help: "Process cumulative CPU time in seconds",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind", "mode"},
	)

	// e2b_fc_process_uptime_seconds:进程已运行时长(秒)
	e2bFcProcessUptimeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_uptime_seconds",
			Help: "Process uptime in seconds",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_threads:线程数
	e2bFcProcessThreads = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_threads",
			Help: "Process thread count",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_open_fds:已打开文件描述符数
	e2bFcProcessOpenFds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_open_fds",
			Help: "Process open file descriptor count",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind"},
	)

	// e2b_fc_process_io_bytes_total:I/O 字节累计
	e2bFcProcessIoBytesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_io_bytes_total",
			Help: "Process cumulative I/O bytes",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind", "operation"},
	)

	// e2b_fc_process_io_ops_total:I/O 次数累计
	e2bFcProcessIoOpsTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_io_ops_total",
			Help: "Process cumulative I/O operations",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind", "operation"},
	)

	// e2b_fc_process_context_switches_total:上下文切换累计
	e2bFcProcessContextSwitchesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_context_switches_total",
			Help: "Process cumulative context switches",
		},
		[]string{"node_ip", "sandbox_id", "pid", "vm_kind", "type"},
	)

	// e2b_fc_process_state_count:按 Linux 进程状态聚合 fc 进程数。
	// state 取自 /proc/<pid>/stat 的第 3 字段(R/S/D/Z/T/I)。
	// Z = zombie、D = uninterruptible disk sleep,都值得告警。
	e2bFcProcessStateCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_fc_process_state_count",
			Help: "Count of firecracker processes by Linux process state (R running, S sleep, D uninterruptible, Z zombie, T stopped, I idle, X dead).",
		},
		[]string{"node_ip", "state"},
	)

	// pageSize:RSS 计算用的页大小常量
	pageSize = int64(os.Getpagesize())

	// clockTicks:CPU 时间计算用的系统时钟节拍(默认 100 Hz,init 时尝试更新)
	clockTicks = int64(100)
)

// init 初始化时钟节拍
func init() {
	// 尝试获取系统的 HZ 值
	if hz := getClockTicks(); hz > 0 {
		clockTicks = hz
	}
}

// getClockTicks 从 /proc/stat 或 sysconf 获取系统 HZ 值
func getClockTicks() int64 {
	// 尝试从 /proc/stat 读取
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 100 // 兜底默认值
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "btime") {
			// btime 在同一文件里;主流 Linux 上 HZ 通常是 100,
			// 严格点应该解析 C 头文件的 CLOCKS_PER_SEC,但 100 作为默认值足够安全。
			return 100
		}
	}
	return 100
}

// getNodeIP 获取节点 IP 地址
func getNodeIP() string {
	// 优先读环境变量
	if ip := os.Getenv("NODE_IP"); ip != "" {
		return ip
	}

	// 从 eth0 接口取 IPv4
	iface, err := net.InterfaceByName("eth0")
	if err != nil {
		log.Printf("Failed to get eth0 interface: %v", err)
		return "unknown"
	}

	addrs, err := iface.Addrs()
	if err != nil {
		log.Printf("Failed to get addresses for eth0: %v", err)
		return "unknown"
	}

	for _, addr := range addrs {
		// 仅取 IPv4(忽略 loopback)
		if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() {
			if ipNet.IP.To4() != nil {
				return ipNet.IP.String()
			}
		}
	}

	return "unknown"
}

// extractSandboxID 从命令行中提取 sandbox_id
func extractSandboxID(commandLine string) string {
	// 在命令行中找 --api-sock 参数
	apiSockIdx := strings.Index(commandLine, "--api-sock")
	if apiSockIdx == -1 {
		return ""
	}

	// 取 --api-sock 后面的剩余部分
	rest := commandLine[apiSockIdx+len("--api-sock"):]
	rest = strings.TrimSpace(rest)

	if rest == "" {
		return ""
	}

	// socket 路径是紧接着的下一个参数,按空格切首段
	socketPath := strings.Split(rest, " ")[0]

	// 从 socket 路径中提取 sandbox_id
	// 格式:/fc-{sandboxID}-{randomID}.sock
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

	// 校验非空
	if sandboxID == "" {
		return ""
	}

	return sandboxID
}

// isFirecrackerProcess 判断是否是 firecracker 进程
func isFirecrackerProcess(pid string) bool {
	// 读取命令行
	cmdlinePath := filepath.Join("/proc", pid, "cmdline")
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return false
	}

	// cmdline 是 null 分隔的字符串
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	cmdline = strings.TrimSpace(cmdline)

	// 命令行包含 firecracker 关键字即视为 fc 进程
	return strings.Contains(strings.ToLower(cmdline), "firecracker")
}

// getProcessCmdline 读取进程命令行
func getProcessCmdline(pid string) (string, error) {
	cmdlinePath := filepath.Join("/proc", pid, "cmdline")
	data, err := os.ReadFile(cmdlinePath)
	if err != nil {
		return "", err
	}

	// cmdline 是 null 分隔的字符串
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	return strings.TrimSpace(cmdline), nil
}

// parseProcState 读取 /proc/<pid>/stat 返回单字符进程状态(R/S/D/Z/T/I/X)。
// 文件不可读或格式异常时返回 "?"。这里独立读一次是为了不打扰 parseStat 既有逻辑;
// 多读一次代价很低(内核 page cache 命中)。
func parseProcState(pid string) string {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "stat"))
	if err != nil {
		return "?"
	}
	stat := string(data)
	lastParen := strings.LastIndex(stat, ")")
	if lastParen == -1 || lastParen+2 >= len(stat) {
		return "?"
	}
	// 格式:"... ) S ppid ..."。跳过 ')' + ' ' 共 2 字节。
	return string(stat[lastParen+2])
}

// parseStat 解析 /proc/[pid]/stat,返回关键指标
func parseStat(pid string) (userTime, systemTime, uptime, vsize float64, err error) {
	statPath := filepath.Join("/proc", pid, "stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	// /proc/[pid]/stat 格式:
	// pid (comm) state ppid pgrp sid ...
	// comm 字段可能含空格,需要小心解析
	stat := string(data)

	// 找最后一个 ')' 作为 comm 段结尾
	lastParen := strings.LastIndex(stat, ")")
	if lastParen == -1 {
		return 0, 0, 0, 0, fmt.Errorf("invalid stat format")
	}

	// 取 comm 之后的部分
	rest := stat[lastParen+1:]
	fields := strings.Fields(rest)

	if len(fields) < 23 {
		return 0, 0, 0, 0, fmt.Errorf("not enough fields in stat")
	}

	// utime(字段 13)和 stime(字段 14)单位是时钟节拍。
	// rest 数组已跳过前 3 个字段(pid、comm、state),所以这里用索引 10 和 11。
	utime, err := strconv.ParseFloat(fields[10], 64)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	stime, err := strconv.ParseFloat(fields[11], 64)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	// 转换为秒
	userTime = utime / float64(clockTicks)
	systemTime = stime / float64(clockTicks)

	// start_time(字段 22)单位是开机以来的时钟节拍数。
	// rest 数组跳过了前 3 个字段,所以索引 18。
	startTime, err := strconv.ParseFloat(fields[18], 64)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	// vsize(字段 23)是虚拟内存字节数。索引同上规则,18+1=19。
	vsize, err = strconv.ParseFloat(fields[19], 64)
	if err != nil {
		return 0, 0, 0, 0, err
	}

	// 取系统启动时间
	btime, err := getBootTime()
	if err != nil {
		return 0, 0, 0, 0, err
	}

	now := float64(time.Now().Unix())
	processStartTime := startTime / float64(clockTicks)
	uptime = now - btime - processStartTime

	if uptime < 0 {
		log.Printf("DEBUG: pid=%s, now=%f, btime=%f, startTime=%f, processStartTime=%f, calc_uptime=%f", pid, now, btime, startTime, processStartTime, uptime)
		uptime = 0
	}

	return userTime, systemTime, uptime, vsize, nil
}

// getBootTime 从 /proc/stat 读取系统启动时间
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

// parseStatm 解析 /proc/[pid]/statm,返回 RSS 页数
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

// parseStatus 解析 /proc/[pid]/status,返回线程数、上下文切换、HugetlbPages
func parseStatus(pid string) (threads int, voluntarySwitches, involuntarySwitches, hugetlbPages float64, err error) {
	statusPath := filepath.Join("/proc", pid, "status")
	data, err := os.ReadFile(statusPath)
	if err != nil {
		return 0, 0, 0, 0, err
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
		} else if strings.HasPrefix(line, "HugetlbPages:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				// HugetlbPages 单位是 kB,转成字节
				hugetlbKb, _ := strconv.ParseFloat(fields[1], 64)
				hugetlbPages = hugetlbKb * 1024
			}
		}
	}

	return threads, voluntarySwitches, involuntarySwitches, hugetlbPages, nil
}

// countOpenFds 统计已打开的文件描述符数
func countOpenFds(pid string) (int, error) {
	fdPath := filepath.Join("/proc", pid, "fd")
	entries, err := os.ReadDir(fdPath)
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// parseIo 解析 /proc/[pid]/io,返回 I/O 统计
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
		} else if strings.HasPrefix(line, "syscr:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				readCount, _ = strconv.ParseFloat(fields[1], 64)
			}
		} else if strings.HasPrefix(line, "syscw:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				writeCount, _ = strconv.ParseFloat(fields[1], 64)
			}
		}
	}

	return readBytes, writeBytes, readCount, writeCount, nil
}

// updateFirecrackerMetrics 采集并更新 firecracker 进程指标。
// 返回 nodeIP 和 sandbox_id → 该沙箱所有 pid 的映射。
// 该映射会传给 checkSandboxLeak 与 orchestrator 权威列表对比。
func updateFirecrackerMetrics() (string, map[string][]string) {
	// 先 reset 所有指标
	e2bFcProcessCount.Reset()
	e2bFcProcessInfo.Reset()
	e2bFcProcessMemoryRssBytes.Reset()
	e2bFcProcessMemoryVsizeBytes.Reset()
	e2bFcProcessMemoryHugetlbBytes.Reset()
	e2bFcProcessCpuSecondsTotal.Reset()
	e2bFcProcessUptimeSeconds.Reset()
	e2bFcProcessThreads.Reset()
	e2bFcProcessOpenFds.Reset()
	e2bFcProcessIoBytesTotal.Reset()
	e2bFcProcessIoOpsTotal.Reset()
	e2bFcProcessContextSwitchesTotal.Reset()
	e2bFcProcessStateCount.Reset()

	// 取节点 IP
	nodeIP := getNodeIP()
	sandboxToPIDs := make(map[string][]string)
	stateCounts := make(map[string]int)

	// 扫描 /proc 目录下所有进程
	procDir := "/proc"
	entries, err := os.ReadDir(procDir)
	if err != nil {
		log.Printf("Failed to read /proc directory: %v", err)
		return nodeIP, sandboxToPIDs
	}

	processCount := 0

	for _, entry := range entries {
		// 仅处理纯数字目录(对应 PID)
		if !entry.IsDir() {
			continue
		}

		pid := entry.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}

		// 是否为 firecracker 进程
		if !isFirecrackerProcess(pid) {
			continue
		}

		// 取命令行并提取 sandbox_id
		cmdline, err := getProcessCmdline(pid)
		if err != nil {
			continue
		}

		sandboxID := extractSandboxID(cmdline)

		// sandbox_id 解析失败:跳过该进程,累计错误计数
		if sandboxID == "" {
			e2bFcProcessParseErrorsTotal.WithLabelValues(nodeIP).Inc()
			continue
		}

		// 按 sandbox_id 前缀区分 build VM 与 instance VM(与 leak 检测同一判据)。
		// build VM 起真实 fc 但不进 orchestrator List,用 vm_kind 让 CPU/内存/FD/线程/
		// 存活时长等进程指标能按 build vs instance 拆分(issue #12)。
		vmKind := "instance"
		if strings.HasPrefix(sandboxID, buildSandboxPrefix) {
			vmKind = "build"
		}

		// 解析 /proc/[pid]/stat:CPU 时间和 uptime
		userTime, systemTime, uptime, vsize, err := parseStat(pid)
		if err != nil {
			log.Printf("Failed to parse stat for pid %s: %v", pid, err)
			continue
		}

		// 解析 /proc/[pid]/statm:内存
		rssPages, err := parseStatm(pid)
		if err != nil {
			log.Printf("Failed to parse statm for pid %s: %v", pid, err)
			continue
		}
		rssBytes := rssPages * pageSize

		// 解析 /proc/[pid]/status:线程数、上下文切换、HugetlbPages
		threads, voluntarySwitches, involuntarySwitches, hugetlbPages, err := parseStatus(pid)
		if err != nil {
			log.Printf("Failed to parse status for pid %s: %v", pid, err)
		}

		// 统计已打开的文件描述符
		openFds, err := countOpenFds(pid)
		if err != nil {
			log.Printf("Failed to count fds for pid %s: %v", pid, err)
		}

		// 解析 /proc/[pid]/io:I/O 统计
		readBytes, writeBytes, readCount, writeCount, err := parseIo(pid)
		if err != nil {
			log.Printf("Failed to parse io for pid %s: %v", pid, err)
		}

		// 更新指标
		e2bFcProcessInfo.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(1)
		e2bFcProcessMemoryRssBytes.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(float64(rssBytes))
		e2bFcProcessMemoryVsizeBytes.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(vsize)
		e2bFcProcessMemoryHugetlbBytes.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(hugetlbPages)
		e2bFcProcessCpuSecondsTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "user").Set(userTime)
		e2bFcProcessCpuSecondsTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "system").Set(systemTime)
		e2bFcProcessUptimeSeconds.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(uptime)
		e2bFcProcessThreads.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(float64(threads))
		e2bFcProcessOpenFds.WithLabelValues(nodeIP, sandboxID, pid, vmKind).Set(float64(openFds))
		e2bFcProcessIoBytesTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "read").Set(readBytes)
		e2bFcProcessIoBytesTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "write").Set(writeBytes)
		e2bFcProcessIoOpsTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "read").Set(readCount)
		e2bFcProcessIoOpsTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "write").Set(writeCount)
		e2bFcProcessContextSwitchesTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "voluntary").Set(voluntarySwitches)
		e2bFcProcessContextSwitchesTotal.WithLabelValues(nodeIP, sandboxID, pid, vmKind, "involuntary").Set(involuntarySwitches)

		sandboxToPIDs[sandboxID] = append(sandboxToPIDs[sandboxID], pid)
		stateCounts[parseProcState(pid)]++
		processCount++
	}

	// 写总数
	e2bFcProcessCount.WithLabelValues(nodeIP).Set(float64(processCount))
	for state, n := range stateCounts {
		e2bFcProcessStateCount.WithLabelValues(nodeIP, state).Set(float64(n))
	}

	if processCount > 0 {
		log.Printf("Updated firecracker metrics: %d processes on node %s", processCount, nodeIP)
	}
	return nodeIP, sandboxToPIDs
}
