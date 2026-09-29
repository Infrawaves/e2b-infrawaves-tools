package main

import (
	"bufio"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 整机实时 CPU 使用率。口径与 orchestrator 准入判断的 cpuPercent 一致:
// 统计整机(所有核聚合)在一段时间窗口内的忙碌占比,而不是单个进程/cgroup。
// orchestrator 在放置沙箱时用这个值和阈值(默认 80%)比较,超过就拒绝放置,
// 因此把它作为独立 Prometheus 指标暴露后,就能画趋势、配"逼近阈值"告警,
// 在 "Failed to place sandbox" 5xx 真正发生之前提前预警。

var (
	// e2b_node_cpu_usage_percent:整机 CPU 使用率(0-100)。
	// 由两次 /proc/stat 采样的差值算出,覆盖两次 scrape 之间的时间窗口。
	nodeCPUUsagePercent = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_cpu_usage_percent",
			Help: "Whole-node CPU utilization percent (0-100), aggregated over all cores. Same semantics as orchestrator placement cpuPercent.",
		},
		[]string{"node_ip"},
	)

	// e2b_node_cpu_cores:整机逻辑核数,便于把使用率换算回核数。
	nodeCPUCores = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_cpu_cores",
			Help: "Number of logical CPU cores on the node (from /proc/stat per-cpu lines).",
		},
		[]string{"node_ip"},
	)
)

// cpuTimes 保存 /proc/stat 聚合 cpu 行的 busy/total 累计值(单位:USER_HZ tick)。
type cpuTimes struct {
	busy  float64
	total float64
	cores int
}

var (
	lastCPUMu   sync.Mutex
	lastCPUStat *cpuTimes
)

// readProcStat 解析 /proc/stat,返回整机聚合 cpu 行的 busy/total 与逻辑核数。
// cpu 行字段顺序:user nice system idle iowait irq softirq steal guest guest_nice
// idle_all = idle + iowait;busy = total - idle_all。
// guest / guest_nice 已计入 user / nice,不重复累加。
func readProcStat() (*cpuTimes, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var ct cpuTimes
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "cpu ") {
			// 聚合行(所有核之和)
			fields := strings.Fields(line)[1:]
			var total, idleAll float64
			for i, fs := range fields {
				v, err := strconv.ParseFloat(fs, 64)
				if err != nil {
					continue
				}
				// 只累加前 8 个字段进 total(user..steal);guest/guest_nice 已含在 user/nice。
				if i < 8 {
					total += v
				}
				// idle 是第 4 个字段(idx 3),iowait 是第 5 个(idx 4)。
				if i == 3 || i == 4 {
					idleAll += v
				}
			}
			ct.total = total
			ct.busy = total - idleAll
		} else if strings.HasPrefix(line, "cpu") {
			// 形如 cpu0 / cpu1 ... 的单核行,数一下核数。
			ct.cores++
		} else {
			// cpu 行都在文件开头,遇到非 cpu 行即可停止。
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return &ct, nil
}

// computeUsagePercent 用两次采样的差值算使用率(0-100)。total 增量非正时返回 -1 表示无效。
func computeUsagePercent(prev, cur *cpuTimes) float64 {
	totalDelta := cur.total - prev.total
	busyDelta := cur.busy - prev.busy
	if totalDelta <= 0 {
		return -1
	}
	pct := busyDelta / totalDelta * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// updateNodeCPUMetrics 采集整机 CPU 使用率。
// 优先用"上次 scrape 快照 → 本次"的差值(窗口更长更平滑);
// 首次采集没有上次快照时,做一次 100ms 短采样兜底,保证首个 scrape 就有值。
func updateNodeCPUMetrics(nodeIP string) {
	cur, err := readProcStat()
	if err != nil {
		log.Printf("cpu: read /proc/stat: %v", err)
		return
	}
	if cur.cores > 0 {
		nodeCPUCores.WithLabelValues(nodeIP).Set(float64(cur.cores))
	}

	lastCPUMu.Lock()
	prev := lastCPUStat
	lastCPUMu.Unlock()

	var pct float64 = -1
	if prev != nil {
		pct = computeUsagePercent(prev, cur)
	}
	if pct < 0 {
		// 首次采集或增量无效:做一次短窗口采样兜底。
		time.Sleep(100 * time.Millisecond)
		cur2, err := readProcStat()
		if err == nil {
			pct = computeUsagePercent(cur, cur2)
			cur = cur2 // 把最新快照存下去,供下次差值使用
		}
	}

	// 保存本次快照供下一轮 scrape 使用。
	lastCPUMu.Lock()
	lastCPUStat = cur
	lastCPUMu.Unlock()

	if pct >= 0 {
		nodeCPUUsagePercent.WithLabelValues(nodeIP).Set(pct)
	}
}
