package connect

// Stage 3 adapter methods on the fork ClientStrategy: the surface the upstream
// platform transport (transport.go, transport_family.go) calls. Upstream grows
// these from its extender directory and delivery scoring, which this tree does
// not carry, so the extender-facing accessors return nil and the H1 dial runs on
// the fork's own evaluation loop.

import (
	"context"
	"net/http"
	"net/netip"
	"time"
)

// DialerInfo describes the dialer a completed dial was won by. The extender ip is
// the zero value for a direct dial, which is what the platform transport reads to
// decide whether the connection is carried by an extender.
type DialerInfo struct {
	Description string
	ExtenderIp  netip.Addr
}

func (self *ClientStrategy) dialerInfo(dialer *clientDialer) *DialerInfo {
	info := &DialerInfo{Description: dialer.String()}
	if dialer.extenderConfig != nil {
		info.ExtenderIp = dialer.extenderConfig.Ip
	}
	return info
}

// H3ExtenderConfig is the extender an h3 carrier would dial through. This tree
// has no quic or dns extender dialers, so there never is one.
func (self *ClientStrategy) H3ExtenderConfig() *ExtenderConfig { return nil }

// ExtenderDirectory is the directory of discovered extenders. This tree has none;
// the type exists so the platform transport can report in-use addresses to it.
type ExtenderDirectory struct{}

func (self *ExtenderDirectory) SetInUse(ip netip.Addr, delta int) {}

func (self *ClientStrategy) ExtenderDirectory() *ExtenderDirectory { return nil }

// ConnectSettings is the connect settings the strategy dials with.
func (self *ClientStrategy) ConnectSettings() *ConnectSettings {
	return &self.settings.ConnectSettings
}

// H1DialContextWithDialer dials the platform url as a framed H1 message
// connection (custom-upgrade negotiation with a fresh WS fallback inside each
// strategy attempt) on the fork's evaluation loop. An authorization denial is
// terminal for the logical dial and does not count against the dialer.
func (self *ClientStrategy) H1DialContextWithDialer(ctx context.Context, address string, requestHeader http.Header, maximum int, enabled bool, stats *H1PlusStats) (H1MessageConn, *DialerInfo, error) {
	causes := newHttpRequestCauses(ctx)
	eval := func(handleCtx context.Context, dialer *clientDialer) *evalResult {
		info := self.dialerInfo(dialer)
		startDial := time.Now()
		conn, err := dialH1MessagesWithinDeadline(handleCtx, address, requestHeader, dialer.WsDialer(self.settings), H1FramerProtocol, maximum, enabled, stats)
		terminal, refusalOnly := httpUpgradeTerminalCauses(err)
		if !refusalOnly {
			dialer.Update(handleCtx, err, time.Since(startDial))
			observeDialAttempt(handleCtx, err)
		}
		return causes.track(&evalResult{h1Conn: conn, dialerInfo: info, err: err, terminal: terminal})
	}
	result, dialErr := self.parallelEvalErr(ctx, eval, false)
	if result == nil {
		return nil, nil, &h1DialError{dial: dialErr, causes: causes.exhausted(ctx, self.ctx)}
	}
	return result.h1Conn, result.dialerInfo, result.err
}

// h1DialError is the error of an H1 dial that produced no result. It reads as the
// strategy's dial error (the *dialTimeoutError the connect governor classifies)
// and also exposes the retained physical causes, which the credential-refusal and
// exhaustion checks inspect.
type h1DialError struct {
	dial   error
	causes error
}

func (self *h1DialError) Error() string {
	if self.dial != nil {
		return self.dial.Error()
	}
	return self.causes.Error()
}

func (self *h1DialError) Unwrap() []error {
	errs := []error{}
	if self.dial != nil {
		errs = append(errs, self.dial)
	}
	if self.causes != nil {
		errs = append(errs, self.causes)
	}
	return errs
}
