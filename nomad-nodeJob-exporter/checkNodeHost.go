package main

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
)

// 节点级容量指标。与 orchestrator 解耦:即使 orchestrator 挂了仍能上报,
// 让 HugeTLB 耗尽 / 磁盘写满这类"fc 启动失败的首要原因"有独立告警来源。

var (
	// hugepages-Nbytes 池:total / free / reserved,按页大小区分。
	// 例如 size_bytes="2097152" 是 2 MiB 页,"1073741824" 是 1 GiB 页。
	nodeHugepagesTotal = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_hugepages_total",
			Help: "Total number of huge pages in the pool (nr_hugepages).",
		},
		[]string{"node_ip", "size_bytes"},
	)

	nodeHugepagesFree = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_hugepages_free",
			Help: "Free huge pages in the pool (free_hugepages).",
		},
		[]string{"node_ip", "size_bytes"},
	)

	nodeHugepagesReserved = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_hugepages_reserved",
			Help: "Reserved (committed but not faulted) huge pages (resv_hugepages).",
		},
		[]string{"node_ip", "size_bytes"},
	)

	nodeDiskFreeBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_disk_free_bytes",
			Help: "Free bytes for the filesystem holding the given path (statfs f_bavail × f_bsize).",
		},
		[]string{"node_ip", "path"},
	)

	nodeDiskTotalBytes = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_node_disk_total_bytes",
			Help: "Total bytes for the filesystem holding the given path (statfs f_blocks × f_bsize).",
		},
		[]string{"node_ip", "path"},
	)
)

const hugepagesRoot = "/sys/kernel/mm/hugepages"

// hugepagesSizeBytes 把 "hugepages-<N>kB" 目录名解析为字节数。
// 名字不符合预期格式时返回 0。
func hugepagesSizeBytes(dirName string) int64 {
	const prefix = "hugepages-"
	const suffix = "kB"
	if !strings.HasPrefix(dirName, prefix) || !strings.HasSuffix(dirName, suffix) {
		return 0
	}
	num := strings.TrimSuffix(strings.TrimPrefix(dirName, prefix), suffix)
	kb, err := strconv.ParseInt(num, 10, 64)
	if err != nil || kb <= 0 {
		return 0
	}
	return kb * 1024
}

func readHugepageCounter(dir, name string) (float64, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

func updateHugepagesMetrics(nodeIP string) {
	entries, err := os.ReadDir(hugepagesRoot)
	if err != nil {
		log.Printf("hugepages: read %s: %v", hugepagesRoot, err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		size := hugepagesSizeBytes(e.Name())
		if size == 0 {
			continue
		}
		dir := filepath.Join(hugepagesRoot, e.Name())
		sizeLabel := strconv.FormatInt(size, 10)

		if v, err := readHugepageCounter(dir, "nr_hugepages"); err == nil {
			nodeHugepagesTotal.WithLabelValues(nodeIP, sizeLabel).Set(v)
		}
		if v, err := readHugepageCounter(dir, "free_hugepages"); err == nil {
			nodeHugepagesFree.WithLabelValues(nodeIP, sizeLabel).Set(v)
		}
		if v, err := readHugepageCounter(dir, "resv_hugepages"); err == nil {
			nodeHugepagesReserved.WithLabelValues(nodeIP, sizeLabel).Set(v)
		}
	}
}

// diskWatchPaths 返回需要 statfs 的路径列表。
// 通过 NODE_DISK_PATHS=/path1:/path2 覆盖(冒号分隔,与 PATH 同风格)。
// 默认监控 rootfs 与 e2b NFS 共享盘。
func diskWatchPaths() []string {
	if v := os.Getenv("NODE_DISK_PATHS"); v != "" {
		var out []string
		for _, p := range strings.Split(v, ":") {
			if p = strings.TrimSpace(p); p != "" {
				out = append(out, p)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{"/", "/mnt/nfs"}
}

func updateDiskMetrics(nodeIP string) {
	for _, p := range diskWatchPaths() {
		var st syscall.Statfs_t
		if err := syscall.Statfs(p, &st); err != nil {
			// 节点上不存在的路径(例如本节点没挂 /mnt/nfs)直接跳过——
			// 不上报任何值,好过假装"剩余 0 字节"被误读成磁盘写满。
			log.Printf("disk: statfs %s: %v", p, err)
			continue
		}
		bsize := int64(st.Bsize)
		if bsize <= 0 {
			continue
		}
		free := int64(st.Bavail) * bsize
		total := int64(st.Blocks) * bsize
		nodeDiskFreeBytes.WithLabelValues(nodeIP, p).Set(float64(free))
		nodeDiskTotalBytes.WithLabelValues(nodeIP, p).Set(float64(total))
	}
}

// updateHostMetrics 是主程序调用的唯一入口,各子系统采集顺序在这里显式排定。
func updateHostMetrics(nodeIP string) {
	if nodeIP == "" {
		nodeIP = "unknown"
	}
	updateHugepagesMetrics(nodeIP)
	updateDiskMetrics(nodeIP)
}

