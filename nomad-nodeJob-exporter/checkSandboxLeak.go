package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
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
// 例外:template build 的 firecracker VM(sandbox_id 以 InstanceBuildPrefix "b" 开头)
// 起真实 fc、socket 命名与用户沙箱相同,但走 template-manager 构建路径、不进
// SandboxService.List,因此永远落在 A\B 里。这类不是泄露,从 leak 中剔除(见 issue #12)。
// build VM 的可见性靠 e2b_fc_process_*{vm_kind="build"}(checkFirecracker.go);卡死用
// e2b_fc_process_uptime_seconds{vm_kind="build"} 在告警侧判,不在 exporter 预聚合。
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

	// --- per-sandbox 配置画像(来自 RunningSandbox 顶层 + 其 deprecated Config 子消息)---
	//
	// A 类(RunningSandbox 顶层,稳定):vcpu / ram_mb / end_time。
	// B 类(RunningSandbox.config,上游标记 deprecated 但当前仍填充):kernel/fc/envd
	// 版本、total_disk_size_mb、auto_pause、allow_internet_access、auto_resume。
	// B 类字段将来某次 orchestrator 升级后可能停止填充而变空(或 0),见 issue #13。

	// 版本画像合成一条 info(value 恒为 1),避免把三个高基数字符串塞进 e2b_sandbox_info
	// 导致其基数翻倍。版本变更本身是沙箱重建,sandbox_id 会变,不会在同一 series 上翻动。
	sandboxBuildInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_build_info",
			Help: "Marker (always 1) carrying per-sandbox build versions from orchestrator config. Empty label = field absent in this orchestrator version.",
		},
		[]string{"node_ip", "sandbox_id", "kernel_version", "firecracker_version", "envd_version"},
	)

	sandboxVcpu = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_vcpu",
			Help: "Configured vCPU count of the sandbox (RunningSandbox.vcpu).",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	sandboxRamMB = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_ram_mb",
			Help: "Configured RAM of the sandbox in MB (RunningSandbox.ram_mb).",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	sandboxDiskMB = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_disk_mb",
			Help: "Configured total disk size of the sandbox in MB (SandboxConfig.total_disk_size_mb, deprecated source).",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	// RunningSandbox.end_time 的 Unix 秒。0 表示未设置(例如无限期沙箱)。
	// 与 age/overrun 不同:overrun 基于 config.max_sandbox_length 推算,end_time 是
	// orchestrator 实际记录的到期时刻,可用于与 age 做一致性校验。
	sandboxEndTimeSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_end_time_seconds",
			Help: "Unix timestamp of RunningSandbox.end_time. 0 means unset (no scheduled expiry).",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	sandboxAllowInternet = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_allow_internet",
			Help: "1 if the sandbox is configured with internet access, else 0 (SandboxConfig.allow_internet_access, deprecated source).",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	sandboxAutoPause = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_auto_pause",
			Help: "1 if the sandbox auto-pauses on timeout, else 0 (SandboxConfig.auto_pause, deprecated source).",
		},
		[]string{"node_ip", "sandbox_id"},
	)

	// auto_resume 超时秒数(SandboxAutoResumeConfig.timeout_seconds)。policy 放进标签。
	// 仅当沙箱携带 auto_resume 子消息时上报;缺失则整条不出(避免 policy="" 噪声)。
	sandboxAutoResumeTimeoutSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_sandbox_auto_resume_timeout_seconds",
			Help: "Auto-resume timeout in seconds (SandboxAutoResumeConfig.timeout_seconds). policy label carries the resume policy.",
		},
		[]string{"node_ip", "sandbox_id", "policy"},
	)
)

const (
	defaultOrchestratorAddr = "127.0.0.1:9090"
	orchestratorListMethod  = "/SandboxService/List"
	orchestratorListTimeout = 3 * time.Second

	// buildSandboxPrefix 是 template build 沙箱 ID 的前缀,对应上游 e2b-dev/infra 的
	// InstanceBuildPrefix("b",见 orchestrator/pkg/template/build/config)。
	// 用户沙箱用 InstanceIDPrefix("i")。build VM 起真实 fc 但不进 SandboxService.List,
	// 故 leak 检测按此前缀剔除(issue #12)。
	buildSandboxPrefix = "b"

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
	// RunningSandbox.end_time(field 4, google.protobuf.Timestamp)
	tagRunningSandboxEndTime = 4
	// RunningSandbox.vcpu(field 8, int64 varint)
	tagRunningSandboxVcpu = 8
	// RunningSandbox.ram_mb(field 9, int64 varint)
	tagRunningSandboxRamMB = 9

	// SandboxConfig.template_id(field 1, string)
	tagSandboxConfigTemplateID = 1
	// SandboxConfig.kernel_version(field 3, string)
	tagSandboxConfigKernelVersion = 3
	// SandboxConfig.firecracker_version(field 4, string)
	tagSandboxConfigFirecrackerVersion = 4
	// SandboxConfig.sandbox_id(field 6, string)
	tagSandboxConfigSandboxID = 6
	// SandboxConfig.envd_version(field 10, string)
	tagSandboxConfigEnvdVersion = 10
	// SandboxConfig.team_id(field 13, string)
	tagSandboxConfigTeamID = 13
	// SandboxConfig.max_sandbox_length(field 14, int64 varint, 小时)
	tagSandboxConfigMaxLength = 14
	// SandboxConfig.total_disk_size_mb(field 15, int64 varint, MB)
	tagSandboxConfigTotalDiskMB = 15
	// SandboxConfig.auto_pause(field 18, bool varint)
	tagSandboxConfigAutoPause = 18
	// SandboxConfig.allow_internet_access(field 21, bool varint)
	tagSandboxConfigAllowInternet = 21
	// SandboxConfig.auto_resume(field 24, SandboxAutoResumeConfig message)
	tagSandboxConfigAutoResume = 24

	// SandboxAutoResumeConfig.policy(field 1, string)
	tagAutoResumePolicy = 1
	// SandboxAutoResumeConfig.timeout_seconds(field 2, uint64 varint)
	tagAutoResumeTimeoutSeconds = 2

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

	// A 类(RunningSandbox 顶层)。EndTimeUnix=0 表示未设置。
	EndTimeUnix int64
	Vcpu        int64
	RamMB       int64

	// B 类(RunningSandbox.config,deprecated 来源)。字符串空 / 数值 0 表示字段缺失。
	KernelVersion       string
	FirecrackerVersion  string
	EnvdVersion         string
	TotalDiskMB         int64
	AutoPause           bool
	AllowInternetAccess bool

	// auto_resume 子消息。HasAutoResume 区分"未携带"与"timeout=0"。
	HasAutoResume     bool
	AutoResumePolicy  string
	AutoResumeTimeout uint64
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

	// 缓存本轮沙箱快照,供 OOM 爆炸半径计算使用(OOM 检测在本函数之前执行,
	// 用上一轮快照即 OOM 发生前一刻该节点承载的沙箱,语义正确)。
	cacheSandboxSnapshot(sandboxes)

	// 建 sandbox_id → record 索引,顺便发布 per-sandbox info。
	now := time.Now().Unix()
	orchestratorIDs := make(map[string]struct{}, len(sandboxes))
	for _, s := range sandboxes {
		if s.SandboxID == "" {
			continue
		}
		orchestratorIDs[s.SandboxID] = struct{}{}
		sandboxInfo.WithLabelValues(nodeIP, s.SandboxID, s.NodeID, s.TemplateID, s.TeamID).Set(1)

		// 版本画像 info(标签缺失即该 orchestrator 版本未填 Config,见 issue #13)。
		sandboxBuildInfo.WithLabelValues(nodeIP, s.SandboxID, s.KernelVersion, s.FirecrackerVersion, s.EnvdVersion).Set(1)

		// 资源画像。vcpu/ram 为 A 类稳定字段;0 视为未知,不上报以免误导容量视图。
		if s.Vcpu > 0 {
			sandboxVcpu.WithLabelValues(nodeIP, s.SandboxID).Set(float64(s.Vcpu))
		}
		if s.RamMB > 0 {
			sandboxRamMB.WithLabelValues(nodeIP, s.SandboxID).Set(float64(s.RamMB))
		}
		if s.TotalDiskMB > 0 {
			sandboxDiskMB.WithLabelValues(nodeIP, s.SandboxID).Set(float64(s.TotalDiskMB))
		}
		if s.EndTimeUnix > 0 {
			sandboxEndTimeSeconds.WithLabelValues(nodeIP, s.SandboxID).Set(float64(s.EndTimeUnix))
		}

		// 布尔开关一律 0/1 上报(deprecated 来源下 false 与"未填"无法区分,统一记 0)。
		sandboxAllowInternet.WithLabelValues(nodeIP, s.SandboxID).Set(boolToFloat(s.AllowInternetAccess))
		sandboxAutoPause.WithLabelValues(nodeIP, s.SandboxID).Set(boolToFloat(s.AutoPause))

		// auto_resume 仅在携带子消息时上报,policy 进标签。
		if s.HasAutoResume {
			sandboxAutoResumeTimeoutSeconds.WithLabelValues(nodeIP, s.SandboxID, s.AutoResumePolicy).Set(float64(s.AutoResumeTimeout))
		}

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

	var leakN, orphanN, consistentN, buildN int
	for sbxID, pids := range fcSandboxes {
		if _, ok := orchestratorIDs[sbxID]; ok {
			consistentN++
			continue
		}
		// build VM(build 前缀)永远不在 orchestrator List 中,属预期而非泄露,
		// 单独计数后跳过,避免 leak=A\B 的系统性误报(issue #12)。
		if strings.HasPrefix(sbxID, buildSandboxPrefix) {
			buildN++
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
	// buildN 仅作 leak 视角的日志旁证(本轮被识别为 build 而从 leak 剔除的数)。

	log.Printf("Sandbox leak summary: consistent=%d leak=%d orphan=%d build=%d (orchestrator=%d, fc=%d) list_rtt=%.3fs",
		consistentN, leakN, orphanN, buildN, len(orchestratorIDs), len(fcSandboxes), dur)
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
		case tagRunningSandboxEndTime:
			ts, tn := protowire.ConsumeBytes(data)
			if tn < 0 {
				return rec, protowire.ParseError(tn)
			}
			secs, err := parseTimestampSeconds(ts)
			if err != nil {
				return rec, err
			}
			rec.EndTimeUnix = secs
			data = data[tn:]
		case tagRunningSandboxVcpu:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return rec, protowire.ParseError(vn)
			}
			rec.Vcpu = int64(v)
			data = data[vn:]
		case tagRunningSandboxRamMB:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return rec, protowire.ParseError(vn)
			}
			rec.RamMB = int64(v)
			data = data[vn:]
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
		case tagSandboxConfigKernelVersion:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.KernelVersion = s
			data = data[sn:]
		case tagSandboxConfigFirecrackerVersion:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.FirecrackerVersion = s
			data = data[sn:]
		case tagSandboxConfigSandboxID:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.SandboxID = s
			data = data[sn:]
		case tagSandboxConfigEnvdVersion:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.EnvdVersion = s
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
		case tagSandboxConfigTotalDiskMB:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return protowire.ParseError(vn)
			}
			rec.TotalDiskMB = int64(v)
			data = data[vn:]
		case tagSandboxConfigAutoPause:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return protowire.ParseError(vn)
			}
			rec.AutoPause = v != 0
			data = data[vn:]
		case tagSandboxConfigAllowInternet:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return protowire.ParseError(vn)
			}
			rec.AllowInternetAccess = v != 0
			data = data[vn:]
		case tagSandboxConfigAutoResume:
			ar, an := protowire.ConsumeBytes(data)
			if an < 0 {
				return protowire.ParseError(an)
			}
			if err := parseAutoResumeConfig(ar, rec); err != nil {
				return err
			}
			rec.HasAutoResume = true
			data = data[an:]
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

// parseAutoResumeConfig 解析 SandboxAutoResumeConfig 子消息:policy(string)+ timeout_seconds(uint64)。
func parseAutoResumeConfig(data []byte, rec *runningSandbox) error {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if n < 0 {
			return protowire.ParseError(n)
		}
		data = data[n:]

		switch int(num) {
		case tagAutoResumePolicy:
			s, sn := protowire.ConsumeString(data)
			if sn < 0 {
				return protowire.ParseError(sn)
			}
			rec.AutoResumePolicy = s
			data = data[sn:]
		case tagAutoResumeTimeoutSeconds:
			v, vn := protowire.ConsumeVarint(data)
			if vn < 0 {
				return protowire.ParseError(vn)
			}
			rec.AutoResumeTimeout = v
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

// boolToFloat 把 Prometheus 布尔语义映射成 0/1。
func boolToFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
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
