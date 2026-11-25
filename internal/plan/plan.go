// Package plan 定义「负载计划」：一次压测实验的全部输入，必须能进版本库、能复现（K1 / H8）。
//
// 一条原则：**实验之间只允许配置不同，不允许代码不同**。
// 所以 persona 分布、流量形状、bot 上限、结果目录都在这里声明，
// 并且在开跑时把计划原文与指纹一起写进结果目录，三个月后还能原样重跑。
package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"

	"aiplayer/swarm/internal/behavior"
	"aiplayer/swarm/internal/sched"
)

// Target 描述被测系统入口。
type Target struct {
	// GatewayURL 是接入层地址，形如 ws://127.0.0.1:8080/ws。
	GatewayURL string `yaml:"gateway_url" json:"gateway_url"`
	// Mode 是匹配模式，默认 ranked5v5。
	Mode string `yaml:"mode" json:"mode"`
	// ClientSecret 是登录签名密钥（自建环境里 bot 与 SUT 共用）。
	ClientSecret string `yaml:"client_secret" json:"client_secret"`
	// Platform 出现在 Hello 里，用于区分 bot 的「设备类型」。
	Platform string `yaml:"platform" json:"platform"`
}

// Behaviors 描述 bot 行为来源。
type Behaviors struct {
	// CatalogFile 是 persona 目录 YAML；留空则使用内置的 6 种原型。
	CatalogFile string `yaml:"catalog_file" json:"catalog_file"`
}

// Limits 是运行约束。
type Limits struct {
	// MaxConcurrentBots 是单 runner 的并发 bot 上限。
	// 这个值直接决定「控制器开销」——它太小会压制负载，太大会让 bot 自己先崩。
	MaxConcurrentBots int `yaml:"max_concurrent_bots" json:"max_concurrent_bots"`
	// MatchWaitTimeoutS 是单次排队最长等待。
	MatchWaitTimeoutS int `yaml:"match_wait_timeout_s" json:"match_wait_timeout_s"`
	// HeartbeatIntervalS 是应用层心跳间隔。
	HeartbeatIntervalS int `yaml:"heartbeat_interval_s" json:"heartbeat_interval_s"`
	// RoomDrainTimeoutS 是等结算的最长时间。
	RoomDrainTimeoutS int `yaml:"room_drain_timeout_s" json:"room_drain_timeout_s"`
}

// SinkCfg 是结果落盘配置。
type SinkCfg struct {
	// Dir 是结果根目录，每次实验一个子目录。
	Dir string `yaml:"dir" json:"dir"`
	// BufferBytes 是写缓冲大小。
	BufferBytes int `yaml:"buffer_bytes" json:"buffer_bytes"`
}

// Plan 是一份完整的负载计划。
type Plan struct {
	Name        string     `yaml:"name" json:"name"`
	Description string     `yaml:"description,omitempty" json:"description,omitempty"`
	Seed        int64      `yaml:"seed" json:"seed"`
	Traffic     sched.Spec `yaml:"traffic" json:"traffic"`
	Target      Target     `yaml:"target" json:"target"`
	Behaviors   Behaviors  `yaml:"behaviors" json:"behaviors"`
	Limits      Limits     `yaml:"limits" json:"limits"`
	Sink        SinkCfg    `yaml:"sink" json:"sink"`
	// SUTConfig 是被测系统配置文件的路径（会一起快照到结果目录）。
	SUTConfig string `yaml:"sut_config,omitempty" json:"sut_config,omitempty"`
}

// Defaults 填充默认值。
func (p *Plan) Defaults() {
	if p.Name == "" {
		p.Name = "unnamed"
	}
	if p.Seed == 0 {
		p.Seed = time.Now().UnixNano()
	}
	if p.Target.Mode == "" {
		p.Target.Mode = "ranked5v5"
	}
	if p.Target.Platform == "" {
		p.Target.Platform = "windows"
	}
	if p.Target.ClientSecret == "" {
		p.Target.ClientSecret = "dsh-swarm-dev-secret"
	}
	if p.Limits.MaxConcurrentBots <= 0 {
		p.Limits.MaxConcurrentBots = 2000
	}
	if p.Limits.MatchWaitTimeoutS <= 0 {
		p.Limits.MatchWaitTimeoutS = 120
	}
	if p.Limits.HeartbeatIntervalS <= 0 {
		p.Limits.HeartbeatIntervalS = 5
	}
	if p.Limits.RoomDrainTimeoutS <= 0 {
		p.Limits.RoomDrainTimeoutS = 120
	}
	if p.Sink.Dir == "" {
		p.Sink.Dir = "results"
	}
	if p.Sink.BufferBytes <= 0 {
		p.Sink.BufferBytes = 256 << 10
	}
	p.Traffic.Defaults()
}

// Validate 校验计划，任何不自洽都在开跑前拦住（而不是压到一半才发现）。
func (p *Plan) Validate() error {
	if p.Target.GatewayURL == "" {
		return fmt.Errorf("plan: target.gateway_url 必填")
	}
	if err := p.Traffic.Validate(); err != nil {
		return fmt.Errorf("plan: %w", err)
	}
	if p.Limits.MaxConcurrentBots > 0 && p.Traffic.TotalBots > p.Limits.MaxConcurrentBots*20 {
		return fmt.Errorf("plan: total_bots=%d 远超并发上限 %d×20，调度会严重滞后（提高 max_concurrent_bots 或降低 total_bots）",
			p.Traffic.TotalBots, p.Limits.MaxConcurrentBots)
	}
	return nil
}

// Load 从 YAML 读取计划。
func Load(path string) (*Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plan: 读取 %s: %w", path, err)
	}
	var p Plan
	if err := yaml.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("plan: 解析 %s: %w", path, err)
	}
	p.Defaults()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Fingerprint 返回计划的稳定指纹，用作实验 ID 的一部分。
//
// 用归一化后的 YAML 原文算 sha256：字段顺序由结构体决定，因此同一份计划
// 无论在哪台机器上算都得到同一个指纹（H8：三个月后还能确认「是不是同一次实验」）。
//
// 【坑】「无论在哪个目录下算都一样」这句话必须靠归一化才成立：
// cmd/runner 在读计划时会把 Sink.Dir / CatalogFile / SUTConfig 解析成**本机绝对路径**，
// 直接哈希整份计划的话，同一份计划在 `D:\work` 和 `/home/me` 下会得到两个指纹，
// 「指纹相同 ⇒ 同一次实验」这个唯一用途就废了。
// 这三个字段只保留**文件名**：既抹掉了机器差异，又保留了「换了哪个 persona 目录 /
// 哪份 SUT 配置」这个真正影响实验结果的信息。
func (p *Plan) Fingerprint() (string, error) {
	b, err := yaml.Marshal(p.fingerprintView())
	if err != nil {
		return "", fmt.Errorf("plan: 计算指纹: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16], nil
}

// fingerprintView 返回用于计算指纹的副本，抹掉与机器相关的路径前缀。
func (p *Plan) fingerprintView() *Plan {
	cp := *p
	cp.Sink.Dir = baseName(p.Sink.Dir)
	cp.Behaviors.CatalogFile = baseName(p.Behaviors.CatalogFile)
	cp.SUTConfig = baseName(p.SUTConfig)
	return &cp
}

// baseName 取路径的最后一段；空串保持空串（表示「用内置默认值」）。
func baseName(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Base(p)
}

// LoadCatalog 加载 persona 目录；未指定文件时用内置默认值。
func (p *Plan) LoadCatalog() (*behavior.Catalog, error) {
	if p.Behaviors.CatalogFile == "" {
		return behavior.Default(), nil
	}
	return behavior.LoadFile(p.Behaviors.CatalogFile)
}

// ResolvePath 把计划里的相对路径按计划文件所在目录解析。
func ResolvePath(baseDir, p string) string {
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(baseDir, p)
}

// ExpectedInterval 返回平均到达间隔，用于 coordinated-omission 修正。
func (p *Plan) ExpectedInterval() time.Duration {
	return p.Traffic.ExpectedInterval()
}
