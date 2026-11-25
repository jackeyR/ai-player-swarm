// Package config 汇总被测系统（SUT）的全部可调参数。
//
// 一个刻意为之的设计：**所有「故意留下的瓶颈」都是配置项**。
// 这样「发现—定位—修复—验证」才能在同一个二进制里跑完，
// 修复前后对比不依赖改代码（方案 §4.4 第 5 条铁律）。
package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"aiplayer/swarm/internal/protocol"
	"aiplayer/swarm/srv/alloc"
	"aiplayer/swarm/srv/gateway"
	"aiplayer/swarm/srv/match"
	"aiplayer/swarm/srv/room"
)

// Redis 是模拟 Redis 的参数。
type Redis struct {
	// Shards 是分片数。
	Shards int `yaml:"shards" json:"shards"`
	// LatencyMs 是模拟往返延迟。
	LatencyMs float64 `yaml:"latency_ms" json:"latency_ms"`
}

// Settlement 是结算写库参数。
type Settlement struct {
	// FsyncMs 是模拟落盘延迟。
	FsyncMs float64 `yaml:"fsync_ms" json:"fsync_ms"`
}

// Config 是 SUT 的完整配置。
type Config struct {
	MetricsAddr       string         `yaml:"metrics_addr" json:"metrics_addr"`
	RoomAdvertiseAddr string         `yaml:"room_advertise_addr" json:"room_advertise_addr"`
	Gateway           gateway.Config `yaml:"gateway" json:"gateway"`
	Room              room.Config    `yaml:"room" json:"room"`
	Alloc             alloc.Config   `yaml:"alloc" json:"alloc"`
	Match             match.Config   `yaml:"match" json:"match"`
	Redis             Redis          `yaml:"redis" json:"redis"`
	Settlement        Settlement     `yaml:"settlement" json:"settlement"`
}

// Defaults 填充默认值。
func (c *Config) Defaults() {
	if c.MetricsAddr == "" {
		c.MetricsAddr = ":9090"
	}
	if c.Redis.Shards <= 0 {
		c.Redis.Shards = 8
	}
	if c.Redis.LatencyMs < 0 {
		c.Redis.LatencyMs = 0
	}
	if c.Settlement.FsyncMs < 0 {
		c.Settlement.FsyncMs = 0
	}
	if c.RoomAdvertiseAddr == "" && c.Gateway.Addr != "" {
		// 默认让房间服监听在 gateway 的同一台机器上，端口 +10。
		c.RoomAdvertiseAddr = "127.0.0.1:8090"
	}
	c.Gateway.Defaults()
	c.Room.Defaults()
	c.Alloc.Defaults()
	c.Match.Defaults()
}

// Validate 校验配置。
func (c *Config) Validate() error {
	if c.Match.TeamSize*2 > protocol.PlayersPerRoom {
		return fmt.Errorf("config: team_size*2=%d 超过房间容量 %d", c.Match.TeamSize*2, protocol.PlayersPerRoom)
	}
	if c.Room.TickRateMs <= 0 {
		return fmt.Errorf("config: room.tick_rate_ms 必须为正")
	}
	if c.Alloc.PoolSize < 0 {
		return fmt.Errorf("config: alloc.pool_size 不能为负")
	}
	return nil
}

// Load 从 YAML 读取配置。
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: 读取 %s: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("config: 解析 %s: %w", path, err)
	}
	c.Defaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Default 返回默认配置（不读文件时用）。
func Default() *Config {
	c := &Config{}
	c.Defaults()
	return c
}
