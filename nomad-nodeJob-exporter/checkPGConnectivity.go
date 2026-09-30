package main

import (
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// PG 连通性探测:从当前节点视角对 PostgreSQL 端点做纯 TCP 拨号(不进入认证流程,
// 因此不需要任何用户名/密码,代码里也不出现任何凭据)。用于回答"api / dashboard-api
// 所在节点此刻能不能连上 PG"这个问题——恰好覆盖 connection refused / 超时 / DNS
// 解析失败这几类故障形态。
//
// 探测目标通过环境变量 PG_CONNECTIVITY_TARGETS 注入,格式为逗号分隔的
// "target=host:port",例如:
//
//	PG_CONNECTIVITY_TARGETS="direct=db.xxxx.supabase.co:5432,pooler=aws-1-ap-northeast-1.pooler.supabase.com:5432"
//
// target 是给这条链路起的可读名字(如 direct / pooler),会成为指标 label,方便在
// Grafana 里对比两条链路。源码里只保留占位默认值,真实地址由 systemd unit 注入。
var (
	// pg_connectivity_up: 1=TCP 可达, 0=不可达
	pgConnectivityUp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pg_connectivity_up",
			Help: "PostgreSQL TCP reachability from this node (1=reachable, 0=unreachable)",
		},
		[]string{"target", "endpoint", "node_ip"},
	)

	// pg_connectivity_latency_seconds: TCP 三次握手建连耗时(秒);不可达时不上报。
	pgConnectivityLatencySeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pg_connectivity_latency_seconds",
			Help: "PostgreSQL TCP connect latency in seconds (only set when reachable)",
		},
		[]string{"target", "endpoint", "node_ip"},
	)

	// pg_connectivity_check_total: 每次探测按结果分类计数,便于观察故障类型分布。
	// result ∈ {ok, refused, timeout, dns_error, other}
	pgConnectivityCheckTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "pg_connectivity_check_total",
			Help: "PostgreSQL TCP connectivity checks by result",
		},
		[]string{"target", "endpoint", "node_ip", "result"},
	)
)

// pgTarget 表示一条待探测的 PG 链路。
type pgTarget struct {
	name     string // 可读名字,如 direct / pooler
	endpoint string // host:port
}

// parsePGTargets 解析 PG_CONNECTIVITY_TARGETS 环境变量。
// 格式:"name1=host1:port1,name2=host2:port2"。解析失败或未设置时返回 nil,
// 由调用方决定是否使用占位默认值。
func parsePGTargets() []pgTarget {
	raw := strings.TrimSpace(os.Getenv("PG_CONNECTIVITY_TARGETS"))
	if raw == "" {
		return nil
	}

	var targets []pgTarget
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, endpoint, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		endpoint = strings.TrimSpace(endpoint)
		if !ok || name == "" || endpoint == "" {
			log.Printf("PG connectivity: skip malformed target %q (want name=host:port)", part)
			continue
		}
		targets = append(targets, pgTarget{name: name, endpoint: endpoint})
	}
	return targets
}

// classifyDialError 把拨号错误归类成 result label,便于按故障类型统计。
func classifyDialError(err error) string {
	if err == nil {
		return "ok"
	}
	// 超时(含 DialTimeout 触发的 i/o timeout)
	var netErr net.Error
	if e, ok := err.(net.Error); ok {
		netErr = e
	}
	if netErr != nil && netErr.Timeout() {
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"):
		return "refused"
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "server misbehaving"),
		strings.Contains(msg, "name resolution"),
		strings.Contains(msg, "lookup "):
		return "dns_error"
	case strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	default:
		return "other"
	}
}

// probePGTarget 对单个端点做一次 TCP 拨号,返回是否可达、耗时、结果分类。
func probePGTarget(endpoint string, timeout time.Duration) (bool, time.Duration, string) {
	start := time.Now()
	conn, err := net.DialTimeout("tcp", endpoint, timeout)
	elapsed := time.Since(start)
	if err != nil {
		return false, elapsed, classifyDialError(err)
	}
	_ = conn.Close()
	return true, elapsed, "ok"
}

// updatePGConnectivityMetrics 是每次 scrape 触发的连通性探测入口。
func updatePGConnectivityMetrics() {
	pgConnectivityUp.Reset()
	pgConnectivityLatencySeconds.Reset()
	// 注意:pgConnectivityCheckTotal 是 Counter,不 Reset(累计语义)。

	targets := parsePGTargets()
	if len(targets) == 0 {
		// 未配置 PG_CONNECTIVITY_TARGETS,跳过探测(不使用任何硬编码真实地址)。
		log.Println("PG connectivity: PG_CONNECTIVITY_TARGETS not set, skip probing")
		return
	}

	nodeIP := getNodeIP()
	timeout := time.Duration(envIntDefault("PG_CONNECTIVITY_TIMEOUT_MS", 3000)) * time.Millisecond

	for _, t := range targets {
		reachable, elapsed, result := probePGTarget(t.endpoint, timeout)
		up := 0.0
		if reachable {
			up = 1.0
			pgConnectivityLatencySeconds.WithLabelValues(t.name, t.endpoint, nodeIP).Set(elapsed.Seconds())
			log.Printf("PG connectivity: target=%s endpoint=%s UP (%.3fs)", t.name, t.endpoint, elapsed.Seconds())
		} else {
			log.Printf("PG connectivity: target=%s endpoint=%s DOWN (result=%s, %.3fs)", t.name, t.endpoint, result, elapsed.Seconds())
		}
		pgConnectivityUp.WithLabelValues(t.name, t.endpoint, nodeIP).Set(up)
		pgConnectivityCheckTotal.WithLabelValues(t.name, t.endpoint, nodeIP, result).Inc()
	}
}
