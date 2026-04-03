package main

import (
	"fmt"
	"log"
	"net"
	"os/exec"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	// node_port_listening: Port listening status
	nodePortListening = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_port_listening",
			Help: "Port listening status (1=listening, 0=not listening)",
		},
		[]string{"port", "node_ip"},
	)
)

// checkPortListening checks if a port is listening
func checkPortListening(port int) bool {
	// Try multiple address formats to cover different binding scenarios
	addresses := []string{
		fmt.Sprintf(":%d", port),        // Any IP address (IPv4/IPv6)
		fmt.Sprintf("0.0.0.0:%d", port), // IPv4 any address
		fmt.Sprintf("[::]:%d", port),    // IPv6 any address
	}

	for _, addr := range addresses {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
	}

	// Try using ss command as a fallback (more reliable than lsof)
	cmd := exec.Command("ss", "-tuln")
	output, err := cmd.Output()
	if err == nil {
		outputStr := string(output)
		// Check if the port appears in the output
		portStr := fmt.Sprintf(":%d", port)
		return strings.Contains(outputStr, portStr)
	}

	// Try using netstat command as another fallback
	cmd = exec.Command("netstat", "-tuln")
	output, err = cmd.Output()
	if err == nil {
		outputStr := string(output)
		// Check if the port appears in the output
		portStr := fmt.Sprintf(":%d", port)
		return strings.Contains(outputStr, portStr)
	}

	return false
}

// updatePortListeningMetrics updates port listening metrics
func updatePortListeningMetrics() {
	// Reset metrics first
	nodePortListening.Reset()

	// Get node IP
	nodeIP := getNodeIP()

	// Check ports
	ports := []int{5016, 9090}
	for _, port := range ports {
		isListening := checkPortListening(port)
		value := 0.0
		if isListening {
			value = 1.0
			log.Printf("Port %d is listening on node %s", port, nodeIP)
		} else {
			log.Printf("Port %d is not listening on node %s", port, nodeIP)
		}
		nodePortListening.WithLabelValues(fmt.Sprintf("%d", port), nodeIP).Set(value)
	}
}
