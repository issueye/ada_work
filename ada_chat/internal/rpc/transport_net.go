package rpc

import (
	"context"
	"net"
)

type netTransport struct{}

// RegisterNet registers the tcp and unix dialers; the chat host is a client only.
func RegisterNet(s *Service) error {
	t := netTransport{}
	if err := s.Register(TCP, t); err != nil {
		return err
	}
	return s.Register(Unix, t)
}

func (netTransport) Dial(ctx context.Context, addr Address) (Stream, error) {
	if addr.Network != TCP && addr.Network != Unix {
		return nil, Unimplemented("rpc-net 不支持传输 " + string(addr.Network))
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, string(addr.Network), addr.Name)
	if err != nil {
		return nil, Unavailable("拨号 " + addr.String() + " 失败: " + err.Error())
	}
	return c, nil
}
