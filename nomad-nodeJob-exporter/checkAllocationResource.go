package main

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
)

// getAllocationResourceInfo 通过 `nomad alloc status -verbose` 解析单个 allocation
// 的 CPU / 内存使用与上限。CLI 输出格式跟 Nomad 版本绑定,升级时需要回归。
func getAllocationResourceInfo(allocID string, nodeID string, nodeName string) (*AllocationResourceInfo, error) {
	cmd := exec.Command("nomad", "alloc", "status", "-verbose", allocID)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if err != nil {
		return nil, err
	}

	output := out.String()
	lines := strings.Split(output, "\n")

	resourceInfo := &AllocationResourceInfo{
		AllocationID: allocID,
		NodeID:       nodeID,
		NodeName:     nodeName,
	}

	var jobName string
	var currentTaskName string

	for _, line := range lines {
		// 提取 Job ID
		if strings.HasPrefix(line, "Job ID") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				jobName = strings.TrimSpace(parts[1])
				resourceInfo.JobName = jobName
			}
		}

		// 提取 Node Name
		if strings.HasPrefix(line, "Node Name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				resourceInfo.NodeName = strings.TrimSpace(parts[1])
			}
		}

		// 进入新的 task 段
		if strings.HasPrefix(line, "Task ") && strings.Contains(line, " is ") {
			// 提取 task 名
			taskNameStart := strings.Index(line, "Task ") + 5
			taskNameEnd := strings.Index(line, " is ")
			if taskNameStart > 0 && taskNameEnd > taskNameStart {
				currentTaskName = strings.Trim(line[taskNameStart:taskNameEnd], "\"")
				resourceInfo.TaskName = currentTaskName
			}
		}

		// "Task Resources:" 标记下一行才是真正的资源数据,本行跳过
		if strings.Contains(line, "Task Resources:") {
			continue
		}

		// 资源数据行(CPU、Memory、Disk)
		if strings.Contains(line, "MHz") && (strings.Contains(line, "MiB") || strings.Contains(line, "GiB")) {
			// 多空格分隔
			fields := strings.Split(line, "  ")

			// 提取 CPU 信息
			for _, field := range fields {
				if strings.Contains(field, "/") && strings.Contains(field, "MHz") {
					fieldForCPU := strings.Split(field, " ")
					cpuParts := strings.Split(fieldForCPU[0], "/")
					if len(cpuParts) == 2 {
						cpuUsageStr := cpuParts[0]
						cpuLimitStr := cpuParts[1]
						cpuUsage, err := strconv.ParseFloat(cpuUsageStr, 64)
						if err == nil {
							resourceInfo.CPUUsage = cpuUsage
						}
						cpuLimit, err := strconv.ParseFloat(cpuLimitStr, 64)
						if err == nil {
							resourceInfo.CPULimit = cpuLimit
						}
					}
					continue
				}
				// 提取 Memory 信息
				if strings.Contains(field, "/") && (strings.Contains(field, "MiB") || strings.Contains(field, "GiB")) {
					if memParts := strings.Split(field, "/"); len(memParts) == 2 {
						memUsageStr := memParts[0]
						memLimitStr := memParts[1]

						resourceInfo.MemoryUsage = parseSize(memUsageStr)
						resourceInfo.MemoryLimit = parseSize(memLimitStr)
					}
					break
				}
				break
			}
		}
	}

	return resourceInfo, nil
}

// parseSize 把 "123 MiB" / "1.5 GiB" 这类字符串归一化为 MiB(float64)。
// 单位识别失败返回 0,**调用方需结合 limit==0 跳过百分比计算**避免除零。
func parseSize(sizeStr string) float64 {
	parts := strings.Fields(sizeStr)
	if len(parts) != 2 {
		return 0
	}

	value, err := strconv.ParseFloat(parts[0], 64)
	if err != nil {
		return 0
	}

	unit := strings.ToLower(parts[1])
	switch unit {
	case "b":
		return value / (1024 * 1024)
	case "kb":
		return value * 1024 / (1024 * 1024)
	case "mb":
		return value
	case "gb":
		return value * 1024
	case "tb":
		return value * 1024 * 1024
	case "kib":
		return value / 1024
	case "mib":
		return value
	case "gib":
		return value * 1024
	case "tib":
		return value * 1024 * 1024
	default:
		return 0
	}
}
