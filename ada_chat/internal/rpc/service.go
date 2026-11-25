package rpc

import (
	"context"
	"time"
)

// 客户端协议上限;服务器端的连接数/在飞/事件上限由 ada_go 的 rpc-json 负责。
const (
	defaultHeartbeat  = 30 * time.Second
	defaultMaxParams  = 1 << 20
	defaultEventQueue = 1024
	minFrame          = 4 << 10
	maxFrameCap       = 64 << 20
)

type config struct {
	heartbeat      time.Duration
	maxFrameBytes  int
	maxParamsBytes int
	maxEventQueue  int
}

func defaultConfig() config {
	return config{
		heartbeat:      defaultHeartbeat,
		maxFrameBytes:  defaultMaxFrame,
		maxParamsBytes: defaultMaxParams,
		maxEventQueue:  defaultEventQueue,
	}
}

func (c *config) normalize() {
	if c.heartbeat <= 0 {
		c.heartbeat = defaultHeartbeat
	}
	if c.maxFrameBytes <= 0 {
		c.maxFrameBytes = defaultMaxFrame
	}
	if c.maxFrameBytes < minFrame {
		c.maxFrameBytes = minFrame
	}
	if c.maxFrameBytes > maxFrameCap {
		c.maxFrameBytes = maxFrameCap
	}
	if c.maxParamsBytes <= 0 {
		c.maxParamsBytes = defaultMaxParams
	}
	if c.maxEventQueue <= 0 {
		c.maxEventQueue = defaultEventQueue
	}
}

// Dial connects to addr and completes the v1 handshake.
func (s *Service) Dial(ctx context.Context, addr Address, opts DialOptions) (Peer, error) {
	tr, ok := s.transports[addr.Network]
	if !ok {
		return nil, Unimplemented("没有注册传输 " + string(addr.Network))
	}
	st, err := tr.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	conf := defaultConfig()
	conf.normalize()
	p, err := newPeer(ctx, st, opts, conf)
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return p, nil
}
