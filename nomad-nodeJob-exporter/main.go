package main

import (
	"bytes"
	"flag"
	"log"
	"net/http"
	"os"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/expfmt"
)

var (
	serviceUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_up",
			Help: "Allocation health status from nomad",
		},
		[]string{"service", "node_id", "node_name", "status", "desired_status"},
	)

	nodeRole = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_node_role",
			Help: "Node role status",
		},
		[]string{"role", "node_id", "node_name"},
	)

	templateroleMetric = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_node_templaterole",
			Help: "Node templaterole status",
		},
		[]string{"templaterole", "node_id", "node_name"},
	)

	allocationCPUUsage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_cpu_usage",
			Help: "Allocation CPU usage in MHz",
		},
		[]string{"allocation_id", "job_name", "task_name", "node_id", "node_name"},
	)

	allocationCPULimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_cpu_limit",
			Help: "Allocation CPU limit in MHz",
		},
		[]string{"allocation_id", "job_name", "task_name", "node_id", "node_name"},
	)

	allocationMemoryUsage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_memory_usage",
			Help: "Allocation memory usage in bytes",
		},
		[]string{"allocation_id", "job_name", "task_name", "node_id", "node_name"},
	)

	allocationMemoryLimit = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_memory_limit",
			Help: "Allocation memory limit in bytes",
		},
		[]string{"allocation_id", "job_name", "task_name", "node_id", "node_name"},
	)

	allocationCPUUsagePercentage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_cpu_usage_percentage",
			Help: "Allocation CPU usage percentage",
		},
		[]string{"allocation_id", "job_name", "task_name", "node_id", "node_name"},
	)

	allocationMemoryUsagePercentage = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "nomad_allocation_memory_usage_percentage",
			Help: "Allocation memory usage percentage",
		},
		[]string{"allocation_id", "job_name", "task_name", "node_id", "node_name"},
	)
)

// updateMetrics 是每次 scrape 触发的总入口,按"先 reset、再采集、再上报"的顺序刷新所有指标。
func updateMetrics() {
	// 先 reset 所有指标(避免上一轮过期 series 残留)
	nodeRole.Reset()
	templateroleMetric.Reset()
	serviceUp.Reset()
	allocationCPUUsage.Reset()
	allocationCPULimit.Reset()
	allocationMemoryUsage.Reset()
	allocationMemoryLimit.Reset()
	allocationCPUUsagePercentage.Reset()
	allocationMemoryUsagePercentage.Reset()
	e2bFcProcessCount.Reset()
	e2bFcProcessParseErrorsTotal.Reset()
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
	nodePortListening.Reset()
	sandboxLeakCount.Reset()
	sandboxOrphanCount.Reset()
	sandboxConsistentCount.Reset()
	sandboxLeak.Reset()
	sandboxOrphan.Reset()
	sandboxInfo.Reset()
	sandboxAgeSeconds.Reset()
	sandboxOverrunSeconds.Reset()
	orchestratorReachable.Reset()
	orchestratorListDurationSeconds.Reset()
	nodeHugepagesTotal.Reset()
	nodeHugepagesFree.Reset()
	nodeHugepagesReserved.Reset()
	nodeDiskFreeBytes.Reset()
	nodeDiskTotalBytes.Reset()

	// 采集 firecracker 进程指标,顺便拿到 sandbox_id → pids 映射用于 leak 检测
	nodeIP, fcSandboxes := updateFirecrackerMetrics()

	// 节点级容量(hugepages、磁盘)——与 orchestrator 解耦,
	// orchestrator 挂了仍然上报,保证容量类告警不被掩盖。
	updateHostMetrics(nodeIP)

	// 与 orchestrator 权威列表比对沙箱状态。
	// orchestrator 不可达(关闭开关 / 服务挂)时静默跳过——
	// leak/orphan 数据不刷新,e2b_orchestrator_reachable 会上报 0。
	if os.Getenv("DISABLE_SANDBOX_LEAK_CHECK") != "1" {
		checkSandboxLeak(nodeIP, fcSandboxes)
	}

	// 更新端口监听指标
	updatePortListeningMetrics()

	// 获取节点信息
	log.Println("Getting node info...")
	nodeInfo, err := getNodeInfo()
	if err != nil {
		log.Println("failed to get node info:", err)
		// 即使取不到 node info,也写一条默认指标说明 exporter 还活着
		serviceUp.WithLabelValues("exporter", "unknown", "unknown", "error", "run").Set(1)
		return
	}

	log.Printf("Got node info: ID=%s, Name=%s", nodeInfo.ID, nodeInfo.Name)

	role := nodeInfo.Meta["role"]
	templaterole := nodeInfo.Meta["templaterole"]

	log.Printf("Node role: %s, templaterole: %s", role, templaterole)

	// 更新 node role 指标
	if role != "" {
		nodeRole.WithLabelValues(role, nodeInfo.ID, nodeInfo.Name).Set(1)
		log.Printf("Node %s (%s) role: %s", nodeInfo.Name, nodeInfo.ID, role)
	} else {
		// 未定义 role 时占位为 unknown
		nodeRole.WithLabelValues("unknown", nodeInfo.ID, nodeInfo.Name).Set(1)
		log.Printf("Node %s (%s) has no role defined", nodeInfo.Name, nodeInfo.ID)
	}

	// 更新 templaterole 指标
	if templaterole != "" {
		templateroleMetric.WithLabelValues(templaterole, nodeInfo.ID, nodeInfo.Name).Set(1)
		log.Printf("Node %s (%s) templaterole: %s", nodeInfo.Name, nodeInfo.ID, templaterole)
	}

	// 按角色巡检必需服务
	log.Println("Checking required services...")
	services, err := checkRequiredServices()
	if err != nil {
		log.Println("failed to check required services:", err)
		// 巡检失败时也写一条占位指标
		serviceUp.WithLabelValues("exporter", nodeInfo.ID, nodeInfo.Name, "error", "run").Set(1)
		return
	}

	// 更新服务健康指标
	log.Printf("Checking required services for node %s (%s):", nodeInfo.Name, nodeInfo.ID)
	if len(services) == 0 {
		log.Println("No required services found for this node")
		// 没有匹配角色对应的必需服务时,占位为正常
		serviceUp.WithLabelValues("exporter", nodeInfo.ID, nodeInfo.Name, "running", "run").Set(1)
	} else {
		for service, allocInfo := range services {
			if allocInfo.IsRunning {
				serviceUp.WithLabelValues(service, nodeInfo.ID, nodeInfo.Name, allocInfo.Status, allocInfo.DesiredStatus).Set(1)
				log.Printf("Service %s: UP (Status: %s, Desired: %s)", service, allocInfo.Status, allocInfo.DesiredStatus)
			} else {
				serviceUp.WithLabelValues(service, nodeInfo.ID, nodeInfo.Name, allocInfo.Status, allocInfo.DesiredStatus).Set(0)
				log.Printf("Service %s: DOWN (Status: %s, Desired: %s)", service, allocInfo.Status, allocInfo.DesiredStatus)
			}
		}
	}

	// 获取 allocations 用于资源监控
	log.Println("Getting allocations for resource monitoring...")
	allocations, err := getAllocations()
	if err != nil {
		log.Println("failed to get allocations:", err)
		return
	}

	// 更新 allocation 资源指标
	for service, allocInfo := range allocations {
		if allocInfo.IsRunning && allocInfo.AllocationID != "" {
			log.Printf("Getting resource info for allocation %s (service: %s)...", allocInfo.AllocationID, service)
			resourceInfo, err := getAllocationResourceInfo(allocInfo.AllocationID, nodeInfo.ID, nodeInfo.Name)
			if err != nil {
				log.Printf("failed to get resource info for allocation %s: %v", allocInfo.AllocationID, err)
				continue
			}

			// 上报资源使用量
			allocationCPUUsage.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.CPUUsage)
			allocationCPULimit.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.CPULimit)
			allocationMemoryUsage.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.MemoryUsage)
			allocationMemoryLimit.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.MemoryLimit)

			// 计算并上报使用率(limit=0 时跳过,避免除零)
			if resourceInfo.CPULimit > 0 {
				cpuPercentage := (resourceInfo.CPUUsage / resourceInfo.CPULimit) * 100
				allocationCPUUsagePercentage.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(cpuPercentage)
				log.Printf("Allocation %s CPU usage: %.2f%% (%.2f MHz/%.2f MHz)", resourceInfo.AllocationID, cpuPercentage, resourceInfo.CPUUsage, resourceInfo.CPULimit)
			}

			if resourceInfo.MemoryLimit > 0 {
				memoryPercentage := (resourceInfo.MemoryUsage / resourceInfo.MemoryLimit) * 100
				allocationMemoryUsagePercentage.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(memoryPercentage)
				log.Printf("Allocation %s Memory usage: %.2f%%", resourceInfo.AllocationID, memoryPercentage)
			}
		}
	}
}

func metricsHandler(w http.ResponseWriter, r *http.Request) {
	updateMetrics()
	promhttp.Handler().ServeHTTP(w, r)
}

func registerMetrics() {
	prometheus.MustRegister(serviceUp)
	prometheus.MustRegister(nodeRole)
	prometheus.MustRegister(templateroleMetric)
	prometheus.MustRegister(allocationCPUUsage)
	prometheus.MustRegister(allocationCPULimit)
	prometheus.MustRegister(allocationMemoryUsage)
	prometheus.MustRegister(allocationMemoryLimit)
	prometheus.MustRegister(allocationCPUUsagePercentage)
	prometheus.MustRegister(allocationMemoryUsagePercentage)
	prometheus.MustRegister(e2bFcProcessCount)
	prometheus.MustRegister(e2bFcProcessParseErrorsTotal)
	prometheus.MustRegister(e2bFcProcessInfo)
	prometheus.MustRegister(e2bFcProcessMemoryRssBytes)
	prometheus.MustRegister(e2bFcProcessMemoryVsizeBytes)
	prometheus.MustRegister(e2bFcProcessMemoryHugetlbBytes)
	prometheus.MustRegister(e2bFcProcessCpuSecondsTotal)
	prometheus.MustRegister(e2bFcProcessUptimeSeconds)
	prometheus.MustRegister(e2bFcProcessThreads)
	prometheus.MustRegister(e2bFcProcessOpenFds)
	prometheus.MustRegister(e2bFcProcessIoBytesTotal)
	prometheus.MustRegister(e2bFcProcessIoOpsTotal)
	prometheus.MustRegister(e2bFcProcessContextSwitchesTotal)
	prometheus.MustRegister(nodePortListening)
	prometheus.MustRegister(sandboxLeakCount)
	prometheus.MustRegister(sandboxOrphanCount)
	prometheus.MustRegister(sandboxConsistentCount)
	prometheus.MustRegister(sandboxLeak)
	prometheus.MustRegister(sandboxOrphan)
	prometheus.MustRegister(sandboxInfo)
	prometheus.MustRegister(sandboxAgeSeconds)
	prometheus.MustRegister(sandboxOverrunSeconds)
	prometheus.MustRegister(orchestratorReachable)
	prometheus.MustRegister(orchestratorListDurationSeconds)
	prometheus.MustRegister(e2bFcProcessStateCount)
	prometheus.MustRegister(nodeHugepagesTotal)
	prometheus.MustRegister(nodeHugepagesFree)
	prometheus.MustRegister(nodeHugepagesReserved)
	prometheus.MustRegister(nodeDiskFreeBytes)
	prometheus.MustRegister(nodeDiskTotalBytes)
}

// printMetrics 用于 -oneshot 模式:跑一遍采集后把指标以 Prometheus 文本格式打到 stdout。
func printMetrics() error {
	// 触发一次采集
	updateMetrics()

	// 收集指标
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return err
	}

	// 以 Prometheus 文本格式写到 stdout
	var buf bytes.Buffer
	encoder := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range metricFamilies {
		if err := encoder.Encode(mf); err != nil {
			return err
		}
	}

	buf.WriteTo(os.Stdout)
	return nil
}

func main() {
	// 解析命令行参数
	oneshot := flag.Bool("oneshot", false, "Run once and print metrics to stdout instead of running as a service")
	flag.Parse()

	registerMetrics()

	if *oneshot {
		if err := printMetrics(); err != nil {
			log.Fatalf("Failed to collect metrics: %v", err)
		}
	} else {
		http.HandleFunc("/metrics", metricsHandler)
		log.Println("exporter started at :9106")
		log.Fatal(http.ListenAndServe(":9106", nil))
	}
}
