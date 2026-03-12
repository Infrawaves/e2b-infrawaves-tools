package main

import (
	"bytes"
	"os/exec"
	"strconv"
	"strings"
)

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
		// Extract job ID
		if strings.HasPrefix(line, "Job ID") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				jobName = strings.TrimSpace(parts[1])
				resourceInfo.JobName = jobName
			}
		}

		// Extract node name
		if strings.HasPrefix(line, "Node Name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				resourceInfo.NodeName = strings.TrimSpace(parts[1])
			}
		}

		// Check if we're starting a new task
		if strings.HasPrefix(line, "Task ") && strings.Contains(line, " is ") {
			// Extract task name
			taskNameStart := strings.Index(line, "Task ") + 5
			taskNameEnd := strings.Index(line, " is ")
			if taskNameStart > 0 && taskNameEnd > taskNameStart {
				currentTaskName = strings.Trim(line[taskNameStart:taskNameEnd], "\"")
				resourceInfo.TaskName = currentTaskName
			}
		}

		// Check if we're at Task Resources section
		if strings.Contains(line, "Task Resources:") {
			// Process the next line which contains the data
			// We'll handle this in the next iteration
			continue
		}

		// Check if this line contains resource data (CPU, Memory, Disk)
		if strings.Contains(line, "MHz") && (strings.Contains(line, "MiB") || strings.Contains(line, "GiB")) {
			// Split by multiple spaces
			fields := strings.Split(line, "  ")

			// Extract CPU info
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
				// Extract Memory info
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
