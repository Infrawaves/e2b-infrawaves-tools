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
		// Extract node ID
		if strings.HasPrefix(line, "ID") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				nodeInfo.ID = strings.TrimSpace(parts[1])
			}
			continue
		}

		// Extract node Name
		if strings.HasPrefix(line, "Name") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 {
				nodeInfo.Name = strings.TrimSpace(parts[1])
			}
			continue
		}

		// Process Meta section
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
		// Check if we've reached the end of the Allocations section
		if inAllocations && (strings.HasPrefix(line, "Attributes") || strings.HasPrefix(line, "Meta")) {
			break
		}

		// Check if we're starting the Allocations section
		if strings.HasPrefix(line, "Allocations") {
			inAllocations = true
			continue
		}

		// Process allocation lines
		if inAllocations {
			fields := strings.Fields(line)
			// Check if this line has enough fields to be an allocation line
			if len(fields) >= 7 {
				// Skip header line
				if fields[0] == "ID" {
					continue
				}
				// Find the task group field (it's the 4th field, index 3)
				taskGroup := fields[3]
				// Find desired status (6th field, index 5)
				desired := fields[5]
				// Find actual status (7th field, index 6)
				status := fields[6]
				// Get allocation ID (1st field, index 0)
				allocID := fields[0]

				isRunning := (desired == "run" && status == "running")

				// Only keep the first occurrence for each task group (newest allocation)
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

	// Check for api role services
	if nodeInfo.Meta["role"] == "api" {
		// requiredServices := []string{"api-service", "client-proxy", "otel-collector", "logs-collector", "loki-service"}
		requiredServices := []string{"api-service", "client-proxy", "otel-collector"}
		for _, service := range requiredServices {
			if alloc, exists := allocations[service]; exists {
				services[service] = alloc
			} else {
				// Service not found, create a placeholder with false status
				services[service] = &AllocationInfo{
					DesiredStatus: "run",
					Status:        "not_found",
					IsRunning:     false,
				}
			}
		}
	}

	// Check for orchestrator role services
	if nodeInfo.Meta["role"] == "orchestrator" {
		// requiredServices := []string{"client-orchestrator", "otel-collector", "logs-collector", "loki-service"}
		requiredServices := []string{"client-orchestrator", "otel-collector"}
		for _, service := range requiredServices {
			if alloc, exists := allocations[service]; exists {
				services[service] = alloc
			} else {
				// Service not found, create a placeholder with false status
				services[service] = &AllocationInfo{
					DesiredStatus: "run",
					Status:        "not_found",
					IsRunning:     false,
				}
			}
		}
	}

	// Check for template-manager role services
	if nodeInfo.Meta["templaterole"] == "template-manager" {
		requiredServices := []string{"template-manager"}
		for _, service := range requiredServices {
			if alloc, exists := allocations[service]; exists {
				services[service] = alloc
			} else {
				// Service not found, create a placeholder with false status
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
