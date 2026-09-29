package main

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// orchestrator 进程 cgroup 内存指标。
// 对应运维脚本里 SSH 到各节点读 /sys/fs/cgroup/<orchestrator-scope>/memory.stat
// 的 anon/file 字段与 memory.max。区别在于:这里由本节点 exporter 直接读本机
// /proc 与 /sys/fs/cgroup,无需 SSH,且随 scrape 进 Prometheus,天然带历史曲线。
//
// 语义说明:
//   - anon:orchestrator 及其子进程(含 firecracker,若归属同一 cgroup 层级)的
//     匿名页常驻内存。注意 firecracker 用 hugepages 时,guest 内存走 HugeTLB,
//     不计入 anon;因此 anon 主要反映 orchestrator 自身 + 非大页的匿名内存。
//   - file:page cache / 映射文件占用,对应脚本里的 file 列(通常几百 GiB,
//     多为 rootfs / 模板文件的缓存,可回收)。
//   - max:该 cgroup 的内存硬上限(memory.max),"max" 表示不限制,上报为 -1。

var (
	orchCgroupAnonBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_anon_bytes",
			Help: "Anonymous memory (memory.stat 'anon') of the orchestrator cgroup scope in bytes.",
		},
		[]string{"node_ip", "cgroup"},
	)

	orchCgroupFileBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_file_bytes",
			Help: "Page cache / file-backed memory (memory.stat 'file') of the orchestrator cgroup scope in bytes.",
		},
		[]string{"node_ip", "cgroup"},
	)

	orchCgroupMaxBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_max_bytes",
			Help: "Memory hard limit (memory.max) of the orchestrator cgroup scope in bytes. -1 means unlimited ('max').",
		},
		[]string{"node_ip", "cgroup"},
	)

	orchCgroupCurrentBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_current_bytes",
			Help: "Current total memory usage (memory.current) of the orchestrator cgroup scope in bytes.",
		},
		[]string{"node_ip", "cgroup"},
	)

	// OOM 计数器,来自 cgroup v2 的 memory.events 文件。这是判断 orchestrator
	// cgroup 是否发生 OOM 的权威信号(anon/current/max 只是用量,无法体现 OOM)。
	//   - oom:因内存不足触发 OOM 处理的次数(memory.events 'oom')。
	//   - oom_kill:实际被 OOM killer 杀死的进程数(memory.events 'oom_kill')——
	//     判断"确实发生 OOM 杀进程"的金标准,只要有增量即代表 cgroup 内有进程被杀。
	// 说明:cgroup 内计数是自 cgroup 创建以来单调累加;cgroup 被重建(如 orchestrator
	// 重启换了 scope)会归零。因此告警侧应观察 increase()/正向 delta,而非绝对值。
	// 以 Gauge 上报文件原始值,把增量判断留给 PromQL,最稳妥。
	orchCgroupOOMTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_oom_total",
			Help: "OOM events (memory.events 'oom') of the orchestrator cgroup scope. Raw cumulative value from cgroup; use increase()/delta to detect new OOM.",
		},
		[]string{"node_ip", "cgroup"},
	)

	orchCgroupOOMKillTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_oom_kill_total",
			Help: "Processes killed by the OOM killer (memory.events 'oom_kill') in the orchestrator cgroup scope. The authoritative signal that an OOM kill happened. Raw cumulative value; use increase()/delta to detect new kills.",
		},
		[]string{"node_ip", "cgroup"},
	)

	// oom_group_kill:整个 cgroup 被作为一组一次性 OOM 杀掉的次数(需内核 >=5.14 且
	// 设置了 memory.oom.group=1)。orchestrator 若配置了 group kill,这个计数比
	// oom_kill 更能代表"orchestrator 及其子进程被整体 OOM 干掉"。字段缺失时不上报。
	orchCgroupOOMGroupKillTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_oom_group_kill_total",
			Help: "Times the whole cgroup was killed as a group by the OOM killer (memory.events 'oom_group_kill'). Raw cumulative value; use increase()/delta to detect new events.",
		},
		[]string{"node_ip", "cgroup"},
	)

	// 值固定为 1,标记本节点是否成功定位到 orchestrator 进程/cgroup,便于告警。
	orchCgroupUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_cgroup_up",
			Help: "1 if the orchestrator process and its cgroup memory files were located and read successfully, else 0.",
		},
		[]string{"node_ip"},
	)
)

// findOrchestratorPID 遍历 /proc,返回 comm/cmdline 匹配 orchestrator 的最小 PID
// (等价于运维脚本里的 `pgrep -o orchestrator`,-o 取最早启动即最小 pid)。
// 找不到返回 ""。
func findOrchestratorPID() string {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		log.Printf("orchestrator-cgroup: read /proc: %v", err)
		return ""
	}

	best := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pidStr := e.Name()
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}

		// 先看 comm(进程名,精确匹配,避免匹配到含 orchestrator 字样的其他进程)
		commData, err := os.ReadFile(filepath.Join("/proc", pidStr, "comm"))
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(commData))
		if comm != "orchestrator" {
			continue
		}

		// 取最小 pid(最早启动)
		if best == 0 || pid < best {
			best = pid
		}
	}

	if best == 0 {
		return ""
	}
	return strconv.Itoa(best)
}

// orchestratorCgroupPath 读 /proc/<pid>/cgroup,返回 cgroup v2 的相对路径
// (格式为 "0::<path>",去掉前缀 "0::")。找不到返回 ""。
func orchestratorCgroupPath(pid string) string {
	data, err := os.ReadFile(filepath.Join("/proc", pid, "cgroup"))
	if err != nil {
		log.Printf("orchestrator-cgroup: read /proc/%s/cgroup: %v", pid, err)
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		// cgroup v2 统一层级形如 "0::/system.slice/....scope"
		if strings.HasPrefix(line, "0::") {
			return strings.TrimSpace(strings.TrimPrefix(line, "0::"))
		}
	}
	return ""
}

// readMemStatField 从 memory.stat 读取指定字段(如 "anon"、"file"),字节数。
func readMemStatField(scope, field string) (float64, bool) {
	data, err := os.ReadFile(filepath.Join(scope, "memory.stat"))
	if err != nil {
		return 0, false
	}
	prefix := field + " "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// readMemScalar 读取 memory.max / memory.current 这类单值文件。
// memory.max 的 "max" 表示无上限,返回 (-1, true)。
func readMemScalar(scope, name string) (float64, bool) {
	data, err := os.ReadFile(filepath.Join(scope, name))
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(data))
	if s == "max" {
		return -1, true
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readMemEventField 从 cgroup v2 的 memory.events 读取指定字段(如 "oom"、"oom_kill")。
// 文件格式为每行 "<key> <value>",例如:
//
//	low 0
//	high 0
//	max 12345
//	oom 3
//	oom_kill 3
//
// 字段不存在或解析失败返回 (0, false)。
func readMemEventField(scope, field string) (float64, bool) {
	data, err := os.ReadFile(filepath.Join(scope, "memory.events"))
	if err != nil {
		return 0, false
	}
	prefix := field + " "
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			v, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, prefix)), 64)
			if err != nil {
				return 0, false
			}
			return v, true
		}
	}
	return 0, false
}

// updateOrchestratorCgroupMetrics 定位本节点 orchestrator 进程并上报其 cgroup 内存指标。
func updateOrchestratorCgroupMetrics(nodeIP string) {
	pid := findOrchestratorPID()
	if pid == "" {
		// 没找到 orchestrator(可能是非 sandbox 节点,或进程未起)——上报 up=0,不写内存指标。
		orchCgroupUp.WithLabelValues(nodeIP).Set(0)
		// 仍驱动 OOM 状态机(found=false):维护布尔标记的自动归零与累计值持续上报。
		updateOrchestratorOOMState(nodeIP, "", 0, false)
		return
	}

	rel := orchestratorCgroupPath(pid)
	if rel == "" {
		orchCgroupUp.WithLabelValues(nodeIP).Set(0)
		updateOrchestratorOOMState(nodeIP, "", 0, false)
		return
	}

	scope := filepath.Join("/sys/fs/cgroup", rel)
	cgLabel := rel

	ok := false
	if v, found := readMemStatField(scope, "anon"); found {
		orchCgroupAnonBytes.WithLabelValues(nodeIP, cgLabel).Set(v)
		ok = true
	}
	if v, found := readMemStatField(scope, "file"); found {
		orchCgroupFileBytes.WithLabelValues(nodeIP, cgLabel).Set(v)
		ok = true
	}
	if v, found := readMemScalar(scope, "memory.max"); found {
		orchCgroupMaxBytes.WithLabelValues(nodeIP, cgLabel).Set(v)
	}
	if v, found := readMemScalar(scope, "memory.current"); found {
		orchCgroupCurrentBytes.WithLabelValues(nodeIP, cgLabel).Set(v)
	}

	// OOM 计数(权威信号)。memory.events 一定与 memory.stat 同目录,
	// 读到即上报原始累计值;字段缺失(极老内核)时静默跳过。
	if v, found := readMemEventField(scope, "oom"); found {
		orchCgroupOOMTotal.WithLabelValues(nodeIP, cgLabel).Set(v)
		ok = true
	}
	oomKill, oomKillFound := readMemEventField(scope, "oom_kill")
	if oomKillFound {
		orchCgroupOOMKillTotal.WithLabelValues(nodeIP, cgLabel).Set(oomKill)
		ok = true
	}
	if v, found := readMemEventField(scope, "oom_group_kill"); found {
		orchCgroupOOMGroupKillTotal.WithLabelValues(nodeIP, cgLabel).Set(v)
		ok = true
	}

	if ok {
		orchCgroupUp.WithLabelValues(nodeIP).Set(1)
	} else {
		orchCgroupUp.WithLabelValues(nodeIP).Set(0)
	}

	// 驱动跨重启 OOM 状态机:用 cgroup 相对路径 rel 作为 scope 稳定标识
	// (orchestrator 重建后该路径会变,据此识别"被杀后拉起")。
	updateOrchestratorOOMState(nodeIP, rel, oomKill, oomKillFound)
}
