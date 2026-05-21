package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/encoding/protowire"
)

// 沙箱泄露 / 孤儿检测 + per-sandbox 身份信息 / 存活时长 / 超 TTL 时长。
//
// 在节点本地比对两个集合:
//   A = /proc 扫描出的 firecracker 进程的 sandbox_id 集合
//   B = orchestrator 通过 gRPC SandboxService.List 返回的 sandbox_id 集合
//
//   leak  = A \ B  → fc 进程存在,但 orchestrator 不知道(僵尸 fc,占资源)
//   orphan = B \ A  → orchestrator 认为有沙箱,但找不到 fc(状态不一致)
//   一致  = A ∩ B
//
// gRPC 直接连本机 orchestrator(默认 127.0.0.1:9090),无 auth。
// 不依赖 protoc 工具链:用 raw codec 直接发空 message,然后用 protowire 手工解析响应中
// 的 RunningSandbox 列表,提取 sandbox_id / client_id / template_id / team_id /
// start_time / max_sandbox_length。

var (
	sandboxLeakCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_leak_count",
			Help: "Number of firecracker processes whose sandbox_id is not present in orchestrator's running list (zombie fc).",
		},
		[]string{"node_ip"},
	)

	// 来自 orchestrator running list 的 per-sandbox 身份标记。
	// 用于跨 sandbox 维度的 dashboard join(sandbox_id × team × template × node)。
	sandboxInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_info",
			Help: "Marker (always 1) for each sandbox known to orchestrator. node_id = orchestrator client_id of the host running the sandbox.",
		},
		[]string{"node_ip", "sandbox_id", "node_id", "template_id", "team_id"},
	)

	// 沙箱已存活秒数 = now - RunningSandbox.start_time。
	sandboxAgeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_age_seconds",
			Help: "Seconds since orchestrator-reported start_time of each running sandbox.",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	// 大于 0 表示沙箱已超过 max_sandbox_length(小时)声明的上限;
	// 0 表示在限额内;max=0 的沙箱(无声明上限)直接跳过不上报。
	sandboxOverrunSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_overrun_seconds",
			Help: "Seconds the sandbox has run past its config.max_sandbox_length. >0 means TTL exceeded — likely stuck or kept alive.",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	sandboxOrphanCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_orphan_count",
			Help: "Number of sandboxes orchestrator claims as running but no firecracker process is found (state divergence).",
		},
		[]string{"node_ip"},
	)

	sandboxConsistentCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_consistent_count",
			Help: "Number of sandboxes present in both /proc and orchestrator (healthy).",
		},
		[]string{"node_ip"},
	)

	sandboxLeak = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_leak",
			Help: "Per-occurrence leak marker (always 1). Use to alert with sandbox_id/pid for kill targeting.",
		},
		[]string{"node_ip", "sandbox_id", "pid"},
	)

	sandboxOrphan = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_orphan",
			Help: "Per-occurrence orphan marker (always 1). Use to alert when orchestrator state is stale.",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	orchestratorReachable = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_reachable",
			Help: "1 if orchestrator gRPC SandboxService.List succeeded this scrape, else 0.",
		},
		[]string{"node_ip"},
	)

	orchestratorListDurationSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_list_duration_seconds",
			Help: "Wall-clock duration of the last SandboxService.List gRPC call (dial + invoke + parse). Only updated on success.",
		},
		[]string{"node_ip"},
	)
)

const (
	defaultOrchestratorAddr = "127.0.0.1:9090"
	orchestratorListMethod  = "/SandboxService/List"
	orchestratorListTimeout = 3 * time.Second

	// Wire-format 字段编号。已与 e2b-dev/infra 仓库的 orchestrator.proto
	// 以及 Nomad 任务定义 orchestrator.hcl(port "grpc" { static = 9090 },
	// GRPC_PORT=9090)交叉比对。
	//
	// SandboxListResponse.sandboxes(field 1, repeated message)
	tagListSandboxes = 1
	// RunningSandbox.config(field 1, message)
	tagRunningSandboxConfig = 1
	// RunningSandbox.client_id(field 2, string)
	tagRunningSandboxClientID = 2
	// RunningSandbox.start_time(field 3, google.protobuf.Timestamp)
	tagRunningSandboxStartTime = 3
	// SandboxConfig.template_id(field 1, string)
	tagSandboxConfigTemplateID = 1
	// SandboxConfig.sandbox_id(field 6, string)
	tagSandboxConfigSandboxID = 6
	// SandboxConfig.team_id(field 13, string)
	tagSandboxConfigTeamID = 13
	// SandboxConfig.max_sandbox_length(field 14, int64 varint, 小时)
	tagSandboxConfigMaxLength = 14
	// google.protobuf.Timestamp.seconds(field 1, int64 varint)
	tagTimestampSeconds = 1

	rawCodecName = "proto" // gRPC 约定 content-subtype 为 "proto"
)

// runningSandbox 是单条 RunningSandbox 抽取出的关键字段。
// 缺失字段为零值;start_time=0 表示未知,这种情况下不上报 age / overrun。
type runningSandbox struct {
	SandboxID      string
	NodeID         string // RunningSandbox.client_id
	TemplateID     string
	TeamID         string
	StartTimeUnix  int64
	MaxLengthHours int64
}

// rawCodec 让 payload 直接以 []byte 形式收发,绕过生成的 stub 用 protowire 手工解析。
// 注册成标准的 "proto" content-subtype,对端 orchestrator 看不出区别。
type rawCodec struct{}

func (rawCodec) Marshal(v any) ([]byte, error) {
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	return nil, fmt.Errorf("rawCodec: expected []byte, got %T", v)
}

func (rawCodec) Unmarshal(data []byte, v any) error {
	dst, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("rawCodec: expected *[]byte, got %T", v)
	}
	out := make([]byte, len(data))
	copy(out, data)
	*dst = out
	return nil
}

func (rawCodec) Name() string { return rawCodecName }

// checkSandboxLeak 调用本机 orchestrator 并更新 leak / orphan / info 指标。
// fcSandboxes 是 sandbox_id → 该沙箱对应的所有 firecracker pid 的映射
// (通常每个 sandbox 只有 1 个 pid,但卡死重启可能留下多个)。
func checkSandboxLeak(nodeIP string, fcSandboxes map[string][]string) {
	addr := os.Getenv("ORCHESTRATOR_ADDR")
	if addr == "" {
		addr = defaultOrchestratorAddr
	}

	start := time.Now()
	sandboxes, err := listOrchestratorSandboxes(addr)
	dur := time.Since(start).Seconds()
	if err != nil {
		log.Printf("Sandbox leak check: orchestrator unreachable at %s: %v", addr, err)
		orchestratorReachable.WithLabelValues(nodeIP).Set(0)
		// 没拿到权威列表时不刷新 leak / orphan 数据——
		// 否则 orchestrator 一挂看起来就像 100% 泄露,误导告警。
		return
	}
	orchestratorReachable.WithLabelValues(nodeIP).Set(1)
	orchestratorListDurationSeconds.WithLabelValues(nodeIP).Set(dur)

	// 建 sandbox_id → record 索引,顺便发布 per-sandbox info。
	now := time.Now().Unix()
	orchestratorIDs := make(map[string]struct{}, len(sandboxes))
	for _, s := range sandboxes {
		if s.SandboxID == "" {
			continue
		}
		orchestratorIDs[s.SandboxID] = struct{}{}
		sandboxInfo.WithLabelValues(nodeIP, s.SandboxID, s.NodeID, s.TemplateID, s.TeamID).Set(1)

		if s.StartTimeUnix > 0 {
			age := now - s.StartTimeUnix
			if age < 0 {
				age = 0
			}
			sandboxAgeSeconds.WithLabelValues(nodeIP, s.SandboxID).Set(float64(age))

			if s.MaxLengthHours > 0 {
				overrun := age - s.MaxLengthHours*3600
				if overrun < 0 {
					overrun = 0
				}
				sandboxOverrunSeconds.WithLabelValues(nodeIP, s.SandboxID).Set(float64(overrun))
			}
		}
	}

	var leakN, orphanN, consistentN int
	for sbxID, pids := range fcSandboxes {
		if _, ok := orchestratorIDs[sbxID]; ok {
			consistentN++
			continue
		}
		leakN++
		for _, pid := range pids {
			sandboxLeak.WithLabelValues(nodeIP, sbxID, pid).Set(1)
		}
		log.Printf("Sandbox leak: sandbox_id=%s pids=%v 不在 orchestrator running list 中", sbxID, pids)
	}
	for sbxID := range orchestratorIDs {
		if _, ok := fcSandboxes[sbxID]; ok {
			continue
		}
		orphanN++
		sandboxOrphan.WithLabelValues(nodeIP, sbxID).Set(1)
		log.Printf("Sandbox orphan: sandbox_id=%s 在 orchestrator 中但本机找不到 fc 进程", sbxID)
	}

	sandboxLeakCount.WithLabelValues(nodeIP).Set(float64(leakN))
	sandboxOrphanCount.WithLabelValues(nodeIP).Set(float64(orphanN))
	sandboxConsistentCount.WithLabelValues(nodeIP).Set(float64(consistentN))

	log.Printf("Sandbox leak summary: consistent=%d leak=%d orphan=%d (orchestrator=%d, fc=%d) list_rtt=%.3fs",
		consistentN, leakN, orphanN, len(orchestratorIDs), len(fcSandboxes), dur)
}

func listOrchestratorSandboxes(addr string) ([]runningSandbox, error) {
	ctx, cancel := context.WithTimeout(context.Background(), orchestratorListTimeout)
	defer cancel()

	cc, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.ForceCodec(rawCodec{})),
	)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer cc.Close()

	var resp []byte
	if err := cc.Invoke(ctx, orchestratorListMethod, []byte{}, &resp); err != nil {
		return nil, fmt.Errorf("invoke %s: %w", orchestratorListMethod, err)
	}

	return parseSandboxListResponse(resp)
}

// parseSandboxListResponse 把 SandboxListResponse protobuf 解码成 per-sandbox 记录数组。
// 未知 / 未来字段通过 protowire.ConsumeFieldValue 跳过,不报错。
func parseSandboxListResponse(data []byte) ([]runningSandbox, error) {
	var out []runningSandbox

	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return nil, fmt.Errorf("consume tag: %w", protowire.ParseError(n))
		}
		data = data[n:]

		if int(num) != tagListSandboxes {
			vn := protowire.ConsumeFieldValue(num, typ, data)
			if vn < 0 {
				return nil, fmt.Errorf("skip field %d: %w", num, protowire.ParseError(vn))
			}
			data = data[vn:]
			continue
		}

		// repeated RunningSandbox:每条都是 length-delimited 的子 message。
		entry, en := protowire.ConsumeBytes(data)
		if en < 0 {
			return nil, fmt.Errorf("consume sandboxes entry: %w", protowire.ParseError(en))
		}
		data = data[en:]

		rec, err := parseRunningSandbox(entry)
		if err != nil {
			return nil, fmt.Errorf("parse running sandbox: %w", err)
		}
		out = append(out, rec)
	}
	return out, nil
}

func parseRunningSandbox(data []byte) (runningSandbox, error) {
	var rec runningSandbox
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return rec, protowire.ParseError(n)
		}
		data = data[n:]

		switch int(num) {
		case tagRunningSandboxConfig:
			cfg, cn := protowire.ConsumeBytes(data)
			if cn < 0 {
				return rec, protowire.ParseError(cn)
			}
			if err := parseSandboxConfig(cfg, &rec); err != nil {
				return rec, err
			}
			data = data[cn:]
		case tagRunningSandboxClientID:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return rec, protowire.ParseError(sn)
			}
			rec.NodeID = s
			data = data[sn:]
		case tagRunningSandboxStartTime:
			ts, tn := protowire.ConsumeBytes(data)
			if tn < 0 {
				return rec, protowire.ParseError(tn)
			}
			secs, err := parseTimestampSeconds(ts)
			if err != nil {
				return rec, err
			}
			rec.StartTimeUnix = secs
			data = data[tn:]
		default:
			vn := protowire.ConsumeFieldValue(num, typ, data)
			if vn < 0 {
				return rec, protowire.ParseError(vn)
			}
			data = data[vn:]
		}
	}
	return rec, nil
}

func parseSandboxConfig(data []byte, rec *runningSandbox) error {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]

		switch int(num) {
		case tagSandboxConfigTemplateID:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.TemplateID = s
			data = data[sn:]
		case tagSandboxConfigSandboxID:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.SandboxID = s
			data = data[sn:]
		case tagSandboxConfigTeamID:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.TeamID = s
			data = data[sn:]
		case tagSandboxConfigMaxLength:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return protowire.ParseError(vn)
			}
			rec.MaxLengthHours = int64(v)
			data = data[vn:]
		default:
			vn := protowire.ConsumeFieldValue(num, typ, data)
			if vn < 0 {
				return protowire.ParseError(vn)
			}
			data = data[vn:]
		}
	}
	return nil
}

func parseTimestampSeconds(data []byte) (int64, error) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return 0, protowire.ParseError(n)
		}
		data = data[n:]

		if int(num) == tagTimestampSeconds {
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return 0, protowire.ParseError(vn)
			}
			return int64(v), nil
		}
		vn := protowire.ConsumeFieldValue(num, typ, data)
		if vn < 0 {
			return 0, protowire.ParseError(vn)
		}
		data = data[vn:]
	}
	return 0, nil
}
