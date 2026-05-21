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
	// node_port_listening: 端口监听状态
	nodePortListening = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_port_listening",
			Help: "Port listening status (1=listening, 0=not listening)",
		},
		[]string{"port", "node_ip"},
	)
)

// checkPortListening 检查端口是否在监听
func checkPortListening(port int) bool {
	// 尝试多种地址格式以兼容不同绑定方式(IPv4-only / IPv6-only / dual-stack)
	addresses := []string{
		fmt.Sprintf(":%d", port),        // 任意 IP(IPv4/IPv6)
		fmt.Sprintf("0.0.0.0:%d", port), // IPv4 任意地址
		fmt.Sprintf("[::]:%d", port),    // IPv6 任意地址
	}

	for _, addr := range addresses {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			conn.Close()
			return true
		}
	}

	// 回退到 ss 命令(比 lsof 更可靠)
	cmd := exec.Command("ss", "-tuln")
	output, err := cmd.Output()
	if err == nil {
		outputStr := string(output)
		// 检查端口是否出现在输出中
		portStr := fmt.Sprintf(":%d", port)
		return strings.Contains(outputStr, portStr)
	}

	// 再回退到 netstat
	cmd = exec.Command("netstat", "-tuln")
	output, err = cmd.Output()
	if err == nil {
		outputStr := string(output)
		// 检查端口是否出现在输出中
		portStr := fmt.Sprintf(":%d", port)
		return strings.Contains(outputStr, portStr)
	}

	return false
}

// updatePortListeningMetrics 更新端口监听指标
func updatePortListeningMetrics() {
	// 先 reset 指标
	nodePortListening.Reset()

	// 获取节点 IP
	nodeIP := getNodeIP()

	// 探测端口
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
