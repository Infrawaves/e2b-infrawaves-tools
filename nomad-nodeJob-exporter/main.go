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

func updateMetrics() {
	// Reset all metrics first
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
	e2bFcProcessCpuSecondsTotal.Reset()
	e2bFcProcessUptimeSeconds.Reset()
	e2bFcProcessThreads.Reset()
	e2bFcProcessOpenFds.Reset()
	e2bFcProcessIoBytesTotal.Reset()
	e2bFcProcessIoOpsTotal.Reset()
	e2bFcProcessContextSwitchesTotal.Reset()

	// Update firecracker metrics
	updateFirecrackerMetrics()

	// Get node info
	log.Println("Getting node info...")
	nodeInfo, err := getNodeInfo()
	if err != nil {
		log.Println("failed to get node info:", err)
		// Even if we can't get node info, set a default metric to indicate the exporter is running
		serviceUp.WithLabelValues("exporter", "unknown", "unknown", "error", "run").Set(1)
		return
	}

	log.Printf("Got node info: ID=%s, Name=%s", nodeInfo.ID, nodeInfo.Name)

	role := nodeInfo.Meta["role"]
	templaterole := nodeInfo.Meta["templaterole"]

	log.Printf("Node role: %s, templaterole: %s", role, templaterole)

	// Update node role metrics
	if role != "" {
		nodeRole.WithLabelValues(role, nodeInfo.ID, nodeInfo.Name).Set(1)
		log.Printf("Node %s (%s) role: %s", nodeInfo.Name, nodeInfo.ID, role)
	} else {
		// Set default role if not found
		nodeRole.WithLabelValues("unknown", nodeInfo.ID, nodeInfo.Name).Set(1)
		log.Printf("Node %s (%s) has no role defined", nodeInfo.Name, nodeInfo.ID)
	}

	// Update templaterole metrics
	if templaterole != "" {
		templateroleMetric.WithLabelValues(templaterole, nodeInfo.ID, nodeInfo.Name).Set(1)
		log.Printf("Node %s (%s) templaterole: %s", nodeInfo.Name, nodeInfo.ID, templaterole)
	}

	// Check required services based on roles
	log.Println("Checking required services...")
	services, err := checkRequiredServices()
	if err != nil {
		log.Println("failed to check required services:", err)
		// Even if we can't check services, set a default metric
		serviceUp.WithLabelValues("exporter", nodeInfo.ID, nodeInfo.Name, "error", "run").Set(1)
		return
	}

	// Update service metrics
	log.Printf("Checking required services for node %s (%s):", nodeInfo.Name, nodeInfo.ID)
	if len(services) == 0 {
		log.Println("No required services found for this node")
		// Set a default metric if no services are found
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

	// Get allocations for resource monitoring
	log.Println("Getting allocations for resource monitoring...")
	allocations, err := getAllocations()
	if err != nil {
		log.Println("failed to get allocations:", err)
		return
	}

	// Update allocation resource metrics
	for service, allocInfo := range allocations {
		if allocInfo.IsRunning && allocInfo.AllocationID != "" {
			log.Printf("Getting resource info for allocation %s (service: %s)...", allocInfo.AllocationID, service)
			resourceInfo, err := getAllocationResourceInfo(allocInfo.AllocationID, nodeInfo.ID, nodeInfo.Name)
			if err != nil {
				log.Printf("failed to get resource info for allocation %s: %v", allocInfo.AllocationID, err)
				continue
			}

			// Update resource usage metrics
			allocationCPUUsage.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.CPUUsage)
			allocationCPULimit.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.CPULimit)
			allocationMemoryUsage.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.MemoryUsage)
			allocationMemoryLimit.WithLabelValues(resourceInfo.AllocationID, resourceInfo.JobName, resourceInfo.TaskName, resourceInfo.NodeID, resourceInfo.NodeName).Set(resourceInfo.MemoryLimit)

			// Calculate and update usage percentages
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
	prometheus.MustRegister(e2bFcProcessCpuSecondsTotal)
	prometheus.MustRegister(e2bFcProcessUptimeSeconds)
	prometheus.MustRegister(e2bFcProcessThreads)
	prometheus.MustRegister(e2bFcProcessOpenFds)
	prometheus.MustRegister(e2bFcProcessIoBytesTotal)
	prometheus.MustRegister(e2bFcProcessIoOpsTotal)
	prometheus.MustRegister(e2bFcProcessContextSwitchesTotal)
}

func printMetrics() error {
	// Update metrics
	updateMetrics()

	// Gather metrics
	metricFamilies, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return err
	}

	// Write metrics to stdout in Prometheus text format
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
	// Parse command line flags
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
