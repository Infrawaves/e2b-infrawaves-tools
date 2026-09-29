package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// 本文件在原始 cgroup OOM 计数(checkOrchestratorCgroup.go 里的
// e2b_orchestrator_cgroup_oom*_total)之上,构建"跨重启"的 OOM 判定与爆炸半径。
//
// 为什么需要:orchestrator 被 OOM 杀死后会被 Nomad 立即重新拉起,新进程落在
// 一个全新的 cgroup scope(scope 路径带 alloc 实例号),原 scope 的 oom_kill
// 计数随之消失、新 scope 从 0 开始。因此单看某一 scope 的 oom_kill 绝对值或
// increase() 会在重启边界丢事件。这里用 exporter 自己的跨 scrape 记忆解决:
//
//   检测信号(满足其一即判定发生了一次 OOM 事件):
//     A. 同一 scope 内 oom_kill 出现正向增量  —— 进程被 OOM 杀,主体可能未重启
//     B. scope 路径发生变化(且非首次观测)     —— orchestrator 整体被杀后重建拉起
//
//   跨重启累计:把"本节点累计 OOM 次数"持久化到磁盘状态文件,exporter 或
//   orchestrator 重启都不清零(A 情况按增量累加,B 情况至少 +1)。
//
//   布尔恢复标记:检测到 OOM 后把 e2b_orchestrator_oom_recovered 置 1,并记录
//   时间戳;持续 oomRecoveredHoldSeconds 秒后自动归 0,形成"上一刻正常→OOM→
//   拉起恢复"这一过程可观测的方波,便于面板高亮与告警。
//
//   爆炸半径:OOM 发生时,用最近一次沙箱快照(OOM 前一刻该节点承载的沙箱)统计
//   受影响沙箱数,并逐个上报 sandbox_id/team_id/template_id 详情。

const (
	// 布尔恢复标记从置 1 到自动归 0 的保持时长。默认 5 分钟,足够 scrape 周期
	// (通常 15~60s)采到方波,又不会长期粘连。可用 OOM_RECOVERED_HOLD_SECONDS 覆盖。
	defaultOOMRecoveredHoldSeconds = 300

	// 爆炸半径详情指标每次 OOM 上报后保持的时长。默认 30 分钟,方便事后排查
	// "那次 OOM 波及了哪些沙箱";过期后清理,避免 series 无限增长。
	defaultOOMBlastHoldSeconds = 1800

	// 每次 OOM 落盘一份爆炸半径快照文件的默认目录。可用 OOM_BLAST_DIR 覆盖。
	// 与内存 series(30 分钟即过期)互补:文件长期留存,供事后按时间检索。
	defaultOOMBlastDir = "/var/lib/e2b-exporter/oom-blasts"
)

var (
	// 跨重启累计:本节点自状态文件建立以来的累计 OOM kill 次数。
	// 以 Gauge 上报持久化累计值(而非 cgroup 原始值),重启不清零。
	orchOOMKillCumulative = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_oom_kill_cumulative_total",
			Help: "Node-local cumulative count of orchestrator cgroup OOM kills, persisted across cgroup re-creation and exporter restarts. Monotonic per node.",
		},
		[]string{"node_ip"},
	)

	// 布尔恢复标记:最近一次 OOM 事件发生后置 1,保持一段时间后自动归 0。
	// 语义:1 = 该节点 orchestrator 近期经历了 OOM 并已(或正在)被拉起恢复。
	orchOOMRecovered = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_oom_recovered",
			Help: "Boolean (1/0). Set to 1 right after an orchestrator OOM event is detected (process killed then restarted by Nomad), auto-resets to 0 after a hold window. Use for edge alerting and dashboard highlight.",
		},
		[]string{"node_ip"},
	)

	// 最近一次 OOM 的 Unix 时间戳(秒)。0/不上报表示从未观测到。
	orchOOMLastTimestamp = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_oom_last_timestamp_seconds",
			Help: "Unix timestamp (seconds) of the most recent detected orchestrator OOM event on this node.",
		},
		[]string{"node_ip"},
	)

	// 最近一次 OOM 事件的爆炸半径:受影响沙箱总数。
	orchOOMBlastSandboxCount = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_oom_blast_sandbox_count",
			Help: "Number of sandboxes running on this node at the moment of the most recent orchestrator OOM (blast radius size).",
		},
		[]string{"node_ip"},
	)

	// 最近一次 OOM 事件的爆炸半径明细:每个受影响沙箱一条(值恒为 1)。
	// 保留 defaultOOMBlastHoldSeconds 后清理。
	orchOOMBlastSandbox = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "e2b_orchestrator_oom_blast_sandbox",
			Help: "Marker (always 1) for each sandbox impacted by the most recent orchestrator OOM on this node. Carries sandbox identity for blast-radius drill-down.",
		},
		[]string{"node_ip", "sandbox_id", "team_id", "template_id"},
	)
)

// oomState 是持久化到磁盘的跨 scrape / 跨重启状态。
type oomState struct {
	// 上次观测到的 orchestrator cgroup scope 相对路径,用于检测重启(scope 变化)。
	LastScope string `json:"last_scope"`
	// 上次在该 scope 观测到的 oom_kill 原始值,用于计算同 scope 内增量。
	LastOOMKill float64 `json:"last_oom_kill"`
	// 跨重启累计的 OOM kill 次数(本节点)。
	Cumulative float64 `json:"cumulative"`
	// 最近一次判定为 OOM 的 Unix 时间戳(秒)。
	LastOOMUnix int64 `json:"last_oom_unix"`
}

var (
	oomStateMu     sync.Mutex
	oomStateOnce   sync.Once
	oomStateMem    oomState // 内存中的当前状态(启动时从磁盘载入)
	oomStateLoaded bool
)

// 最近一次沙箱快照,由 checkSandboxLeak 每轮写入,OOM 爆炸半径读取。
var (
	lastSandboxMu   sync.Mutex
	lastSandboxSnap []runningSandbox
)

// cacheSandboxSnapshot 由 checkSandboxLeak 在拿到权威沙箱列表后调用,
// 缓存一份浅拷贝供 OOM 爆炸半径使用。
func cacheSandboxSnapshot(sandboxes []runningSandbox) {
	lastSandboxMu.Lock()
	defer lastSandboxMu.Unlock()
	cp := make([]runningSandbox, len(sandboxes))
	copy(cp, sandboxes)
	lastSandboxSnap = cp
}

// snapshotSandboxes 返回最近一轮沙箱快照的拷贝。
func snapshotSandboxes() []runningSandbox {
	lastSandboxMu.Lock()
	defer lastSandboxMu.Unlock()
	cp := make([]runningSandbox, len(lastSandboxSnap))
	copy(cp, lastSandboxSnap)
	return cp
}

// oomRecoveredHoldSeconds / oomBlastHoldSeconds 读取可选 env 覆盖。
func oomRecoveredHoldSeconds() int64 {
	return envIntDefault("OOM_RECOVERED_HOLD_SECONDS", defaultOOMRecoveredHoldSeconds)
}

func oomBlastHoldSeconds() int64 {
	return envIntDefault("OOM_BLAST_HOLD_SECONDS", defaultOOMBlastHoldSeconds)
}

// oomStateFilePath 返回状态文件路径。可用 OOM_STATE_FILE 覆盖;
// 默认放到 /var/lib/e2b-exporter/oom_state.json(需要该目录可写),
// 若创建失败则回退到系统临时目录,保证 exporter 不因此启动失败。
func oomStateFilePath() string {
	if v := os.Getenv("OOM_STATE_FILE"); v != "" {
		return v
	}
	dir := "/var/lib/e2b-exporter"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fallback := filepath.Join(os.TempDir(), "e2b-exporter")
		if mkErr := os.MkdirAll(fallback, 0o755); mkErr != nil {
			// 两处都建不了时,直接用临时目录根,尽量不阻断。
			return filepath.Join(os.TempDir(), "e2b_oom_state.json")
		}
		return filepath.Join(fallback, "oom_state.json")
	}
	return filepath.Join(dir, "oom_state.json")
}

// loadOOMState 启动时从磁盘载入一次状态(幂等)。
func loadOOMState() {
	oomStateOnce.Do(func() {
		path := oomStateFilePath()
		data, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				log.Printf("oom-state: read %s: %v (starting fresh)", path, err)
			}
			oomStateLoaded = true
			return
		}
		var s oomState
		if err := json.Unmarshal(data, &s); err != nil {
			log.Printf("oom-state: parse %s: %v (starting fresh)", path, err)
			oomStateLoaded = true
			return
		}
		oomStateMem = s
		oomStateLoaded = true
		log.Printf("oom-state: loaded from %s (cumulative=%.0f last_scope=%q)", path, s.Cumulative, s.LastScope)
	})
}

// persistOOMState 把内存状态写回磁盘(原子替换)。调用方需持有 oomStateMu。
func persistOOMState() {
	path := oomStateFilePath()
	data, err := json.Marshal(oomStateMem)
	if err != nil {
		log.Printf("oom-state: marshal: %v", err)
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Printf("oom-state: write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("oom-state: rename %s->%s: %v", tmp, path, err)
	}
}

// updateOrchestratorOOMState 是 OOM 状态机主入口,由 cgroup 采集在读到
// scope 与 oom_kill 原始值后调用。scope 为空 / 读取失败时 found=false,
// 此时只维护布尔标记的自动归零,不改累计。
//
// 参数:
//
//	nodeIP   本节点 IP
//	scope    orchestrator cgroup 相对路径(检测重启用);found=false 时忽略
//	oomKill  当前 scope 的 memory.events oom_kill 原始值;found=false 时忽略
//	found    是否成功读到 scope 与 oom_kill
func updateOrchestratorOOMState(nodeIP, scope string, oomKill float64, found bool) {
	loadOOMState()

	oomStateMu.Lock()
	defer oomStateMu.Unlock()

	now := time.Now().Unix()
	oomDetected := false
	var delta float64

	if found {
		switch {
		case oomStateMem.LastScope == "":
			// 首次观测:仅登记基线,不判定 OOM(避免把历史累计误当成一次新事件)。
			oomStateMem.LastScope = scope
			oomStateMem.LastOOMKill = oomKill

		case scope == oomStateMem.LastScope:
			// 同一 scope:比较增量。oom_kill 单调递增,正向增量即新发生的 OOM kill。
			if oomKill > oomStateMem.LastOOMKill {
				delta = oomKill - oomStateMem.LastOOMKill
				oomDetected = true
			}
			oomStateMem.LastOOMKill = oomKill

		default:
			// scope 变了:orchestrator 被重建(多因被杀后 Nomad 重新拉起)。
			// 这既包含 OOM 导致的重启,也可能是普通重启/重调度。为不漏报 OOM,
			// 这里保守判定为一次 OOM 事件并至少累加新 scope 观测到的 oom_kill;
			// 若新 scope oom_kill 为 0(重启后还没再 OOM),仍按"发生过一次整体被杀"+1。
			if oomKill > 0 {
				delta = oomKill
			} else {
				delta = 1
			}
			oomDetected = true
			oomStateMem.LastScope = scope
			oomStateMem.LastOOMKill = oomKill
		}
	}

	if oomDetected {
		oomStateMem.Cumulative += delta
		oomStateMem.LastOOMUnix = now
		persistOOMState()
		// 计算并上报爆炸半径(用 OOM 前一刻的沙箱快照)。
		publishBlastRadius(nodeIP, now)
		// 同时把该快照落盘为独立文件,长期留存供事后按时间检索。
		writeBlastSnapshotFile(nodeIP, scope, delta, oomStateMem.Cumulative, time.Unix(now, 0), snapshotSandboxes())
		log.Printf("oom-detected: node=%s scope=%q delta=%.0f cumulative=%.0f", nodeIP, scope, delta, oomStateMem.Cumulative)
	}

	// 累计值每轮都上报(即使本轮无新事件),保证曲线连续。
	orchOOMKillCumulative.WithLabelValues(nodeIP).Set(oomStateMem.Cumulative)

	// 最近一次 OOM 时间戳。
	if oomStateMem.LastOOMUnix > 0 {
		orchOOMLastTimestamp.WithLabelValues(nodeIP).Set(float64(oomStateMem.LastOOMUnix))
	}

	// 布尔恢复标记:距最近一次 OOM 在保持窗口内则为 1,否则 0。
	if oomStateMem.LastOOMUnix > 0 && now-oomStateMem.LastOOMUnix <= oomRecoveredHoldSeconds() {
		orchOOMRecovered.WithLabelValues(nodeIP).Set(1)
	} else {
		orchOOMRecovered.WithLabelValues(nodeIP).Set(0)
	}

	// 爆炸半径明细过期清理:超过保持窗口后不再重发(Reset 由 main 每轮统一做,
	// 这里只在仍处于窗口内时重发,保证 series 在窗口内持续存在)。
	if oomStateMem.LastOOMUnix > 0 && now-oomStateMem.LastOOMUnix <= oomBlastHoldSeconds() && !oomDetected {
		republishBlastRadius(nodeIP)
	}
}

// blastSnapshot 缓存最近一次 OOM 的爆炸半径明细,用于在保持窗口内每轮重发
// (因为 main 每轮 Reset 所有指标)。
var (
	blastMu        sync.Mutex
	blastCount     float64
	blastSandboxes []runningSandbox
)

// publishBlastRadius 在检测到 OOM 时计算爆炸半径并上报。
func publishBlastRadius(nodeIP string, _ int64) {
	sandboxes := snapshotSandboxes()

	blastMu.Lock()
	blastSandboxes = sandboxes
	blastCount = float64(len(sandboxes))
	blastMu.Unlock()

	orchOOMBlastSandboxCount.WithLabelValues(nodeIP).Set(float64(len(sandboxes)))
	for _, s := range sandboxes {
		if s.SandboxID == "" {
			continue
		}
		orchOOMBlastSandbox.WithLabelValues(nodeIP, s.SandboxID, s.TeamID, s.TemplateID).Set(1)
	}
}

// blastSandboxRecord 是快照文件里单个沙箱的完整记录。
type blastSandboxRecord struct {
	SandboxID      string `json:"sandbox_id"`
	TeamID         string `json:"team_id"`
	TemplateID     string `json:"template_id"`
	NodeID         string `json:"node_id"`                   // orchestrator client_id
	StartTimeUnix  int64  `json:"start_time_unix,omitempty"` // 沙箱启动时刻
	MaxLengthHours int64  `json:"max_length_hours,omitempty"`
}

// blastSnapshotFile 是每次 OOM 落盘的爆炸半径快照文件结构(JSON)。
type blastSnapshotFile struct {
	NodeIP       string               `json:"node_ip"`
	DetectedAt   string               `json:"detected_at"`    // RFC3339(UTC)可读时间
	DetectedUnix int64                `json:"detected_unix"`  // Unix 秒,便于按时间检索
	Scope        string               `json:"scope"`          // 触发时的 orchestrator cgroup 路径
	OOMKillDelta float64              `json:"oom_kill_delta"` // 本次事件新增的 oom_kill(scope 变化时可能为约定值 1)
	Cumulative   float64              `json:"cumulative"`     // 本节点累计 OOM 次数(落盘时刻)
	SandboxCount int                  `json:"sandbox_count"`  // 受影响沙箱总数
	Sandboxes    []blastSandboxRecord `json:"sandboxes"`      // 受影响沙箱明细
}

// oomBlastDir 返回快照文件目录(可用 OOM_BLAST_DIR 覆盖)。
func oomBlastDir() string {
	if v := os.Getenv("OOM_BLAST_DIR"); v != "" {
		return v
	}
	return defaultOOMBlastDir
}

// writeBlastSnapshotFile 在检测到 OOM 时,把 OOM 前一刻的沙箱快照落盘为一个独立文件。
// 文件名形如 oom-<nodeIP>-<UTC时间戳>.json,便于后续按节点/时间检索。
// 写盘失败只记日志,不影响指标上报与状态机。
func writeBlastSnapshotFile(nodeIP, scope string, delta, cumulative float64, at time.Time, sandboxes []runningSandbox) {
	dir := oomBlastDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("oom-blast-file: mkdir %s: %v (skip snapshot file)", dir, err)
		return
	}

	recs := make([]blastSandboxRecord, 0, len(sandboxes))
	for _, s := range sandboxes {
		if s.SandboxID == "" {
			continue
		}
		recs = append(recs, blastSandboxRecord{
			SandboxID:      s.SandboxID,
			TeamID:         s.TeamID,
			TemplateID:     s.TemplateID,
			NodeID:         s.NodeID,
			StartTimeUnix:  s.StartTimeUnix,
			MaxLengthHours: s.MaxLengthHours,
		})
	}

	payload := blastSnapshotFile{
		NodeIP:       nodeIP,
		DetectedAt:   at.UTC().Format(time.RFC3339),
		DetectedUnix: at.Unix(),
		Scope:        scope,
		OOMKillDelta: delta,
		Cumulative:   cumulative,
		SandboxCount: len(recs),
		Sandboxes:    recs,
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		log.Printf("oom-blast-file: marshal: %v", err)
		return
	}

	// 文件名:oom-<nodeIP>-<UTC compact 时间戳>.json。nodeIP 里的冒号/点保留即可
	// (Linux 文件名允许);为稳妥把冒号替换为下划线。
	safeIP := strings.ReplaceAll(nodeIP, ":", "_")
	name := "oom-" + safeIP + "-" + at.UTC().Format("20060102T150405Z") + ".json"
	full := filepath.Join(dir, name)

	// 极小概率同一秒内两次 OOM 撞名:附加纳秒后缀去重。
	if _, statErr := os.Stat(full); statErr == nil {
		name = "oom-" + safeIP + "-" + at.UTC().Format("20060102T150405Z") + "-" + strconv.FormatInt(int64(at.Nanosecond()), 10) + ".json"
		full = filepath.Join(dir, name)
	}

	if err := os.WriteFile(full, data, 0o644); err != nil {
		log.Printf("oom-blast-file: write %s: %v", full, err)
		return
	}
	log.Printf("oom-blast-file: wrote %s (%d sandboxes)", full, len(recs))

	// 可选:按保留天数清理超期快照文件(默认不清理,长期留存)。
	if days := envIntDefault("OOM_BLAST_FILE_RETENTION_DAYS", 0); days > 0 {
		pruneOldBlastFiles(dir, days)
	}
}

// pruneOldBlastFiles 删除 dir 下修改时间早于 days 天前的 oom-*.json 快照文件。
func pruneOldBlastFiles(dir string, days int64) {
	cutoff := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "oom-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// republishBlastRadius 在保持窗口内每轮重发上次的爆炸半径(main 每轮 Reset)。
func republishBlastRadius(nodeIP string) {
	blastMu.Lock()
	defer blastMu.Unlock()
	orchOOMBlastSandboxCount.WithLabelValues(nodeIP).Set(blastCount)
	for _, s := range blastSandboxes {
		if s.SandboxID == "" {
			continue
		}
		orchOOMBlastSandbox.WithLabelValues(nodeIP, s.SandboxID, s.TeamID, s.TemplateID).Set(1)
	}
}

// envIntDefault 读取整数型环境变量,缺失或非法时返回默认值。
func envIntDefault(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
