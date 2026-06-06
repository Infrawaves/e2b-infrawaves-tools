package main

import (
	"bytes"
	"os/exec"
	"strings"
)

type NodeInfo struct {
	ID   string
	Name string
	Meta map[string]string
}

type AllocationInfo struct {
	DesiredStatus string
	Status        string
	IsRunning     bool
	AllocationID  string
}

type AllocationResourceInfo struct {
	AllocationID string
	JobName      string
	TaskName     string
	CPUUsage     float64
	CPULimit     float64
	MemoryUsage  float64
	MemoryLimit  float64
	DiskUsage    float64
	DiskLimit    float64
	NodeID       string
	NodeName     string
}

// getNodeInfo 通过 `nomad node status -verbose -self` 取本节点的 ID / Name / Meta。
// Meta 里包含 role、templaterole 等业务标签,后续巡检 required services 用。
func getNodeInfo() (*NodeInfo, error) {
	cmd := exec.Command("nomad", "node", "status", "-verbose", "-self")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}

	output := out.String()
	lines := strings.Split(output, "\n")

	nodeInfo := &NodeInfo{
		Meta: make(map[string]string),
	}

	inMeta := false
	for _, line := range lines {
		// 提取 node ID
		if strings.HasPrefix(line, "ID") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				nodeInfo.ID = strings.TrimSpace(parts[1])
			}
			continue
		}

		// 提取 node Name
		if strings.HasPrefix(line, "Name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				nodeInfo.Name = strings.TrimSpace(parts[1])
			}
			continue
		}

		// 进入 Meta 段
		if strings.HasPrefix(line, "Meta") {
			inMeta = true
			continue
		}

		if inMeta && strings.TrimSpace(line) == "" {
			break
		}

		if inMeta {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				key := strings.TrimSpace(parts[0])
				value := strings.TrimSpace(parts[1])
				nodeInfo.Meta[key] = value
			}
		}
	}

	return nodeInfo, nil
}

// getAllocations 从同一 `nomad node status` 输出里抓 Allocations 段,
// 返回 task group → AllocationInfo 的映射。同一 task group 多次分配时只保留最新一次。
func getAllocations() (map[string]*AllocationInfo, error) {
	cmd := exec.Command("nomad", "node", "status", "-verbose", "-self")
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}

	output := out.String()
	lines := strings.Split(output, "\n")

	allocations := make(map[string]*AllocationInfo)

	inAllocations := false
	for _, line := range lines {
		// Allocations 段的结束:遇到下一段标题
		if inAllocations && (strings.HasPrefix(line, "Attributes") || strings.HasPrefix(line, "Meta")) {
			break
		}

		// Allocations 段开始
		if strings.HasPrefix(line, "Allocations") {
			inAllocations = true
			continue
		}

		// 解析 allocation 行
		if inAllocations {
			fields := strings.Fields(line)
			// 字段够多才视为 allocation 行
			if len(fields) >= 7 {
				// 跳过表头行
				if fields[0] == "ID" {
					continue
				}
				// task group 字段(第 4 列,索引 3)
				taskGroup := fields[3]
				// desired status(第 6 列,索引 5)
				desired := fields[5]
				// 实际 status(第 7 列,索引 6)
				status := fields[6]
				// allocation ID(第 1 列,索引 0)
				allocID := fields[0]

				isRunning := (desired == "run" && status == "running")

				// 同一 task group 只保留首次出现的(对应最新 allocation)
				if _, exists := allocations[taskGroup]; !exists {
					allocations[taskGroup] = &AllocationInfo{
						DesiredStatus: desired,
						Status:        status,
						IsRunning:     isRunning,
						AllocationID:  allocID,
					}
				}
			}
		}
	}

	return allocations, nil
}

// checkRequiredServices 按节点 role / templaterole 巡检对应角色应当运行的关键服务,
// 缺失服务以 status="not_found" 占位返回。新增角色或调整必需服务列表都要改这里。
func checkRequiredServices() (map[string]*AllocationInfo, error) {
	nodeInfo, err := getNodeInfo()
	if err != nil {
		return nil, err
	}

	allocations, err := getAllocations()
	if err != nil {
		return nil, err
	}

	services := make(map[string]*AllocationInfo)

	// api role 必需服务
	if nodeInfo.Meta["role"] == "api" {
		// 早期版本含 logs-collector / loki-service,为兼容未部署日志栈的客户环境已精简
		// requiredServices := []string{"api-service", "client-proxy", "otel-collector", "logs-collector", "loki-service"}
		requiredServices := []string{"api-service", "client-proxy", "otel-collector"}
		for _, service := range requiredServices {
			if alloc, exists := allocations[service]; exists {
				services[service] = alloc
			} else {
				// 服务未找到,以 not_found 占位
				services[service] = &AllocationInfo{
					DesiredStatus: "run",
					Status:        "not_found",
					IsRunning:     false,
				}
			}
		}
	}

	// orchestrator role 必需服务
	if nodeInfo.Meta["role"] == "orchestrator" {
		// 早期版本同样含日志栈,已精简
		// requiredServices := []string{"client-orchestrator", "otel-collector", "logs-collector", "loki-service"}
		requiredServices := []string{"otel-collector"}
		for _, service := range requiredServices {
			if alloc, exists := allocations[service]; exists {
				services[service] = alloc
			} else {
				// 服务未找到,以 not_found 占位
				services[service] = &AllocationInfo{
					DesiredStatus: "run",
					Status:        "not_found",
					IsRunning:     false,
				}
			}
		}
	}

	// template-manager role 必需服务
	if nodeInfo.Meta["templaterole"] == "template-manager" {
		requiredServices := []string{"template-manager"}
		for _, service := range requiredServices {
			if alloc, exists := allocations[service]; exists {
				services[service] = alloc
			} else {
				// 服务未找到,以 not_found 占位
				services[service] = &AllocationInfo{
					DesiredStatus: "run",
					Status:        "not_found",
					IsRunning:     false,
				}
			}
		}
	}

	return services, nil
}
