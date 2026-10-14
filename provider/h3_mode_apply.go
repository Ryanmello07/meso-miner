package main

import (
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
)

// h3AppliedMode is the mode last applied through the h3 control key, or -1 before
// the first application (the startup replay), which must not restart anything.
var h3AppliedMode atomic.Int32

func init() { h3AppliedMode.Store(-1) }

// activeProxyReloader is the running provider's reloader, for the few control
// changes that must relaunch the direct identity.
var activeProxyReloader atomic.Pointer[ProxyReloader]

// h3DefaultModeName is the mode a cleared h3 key returns to: what a restart
// would use, so the live state matches the persisted one.
func h3DefaultModeName() string {
	if h3Enabled() {
		return h3ModeOnName
	}
	return h3ModeDirectName
}

// applyH3Mode applies an h3 mode to the running provider. The runtime gate follows
// the mode at once. The transport mode policy is fixed when an identity builds its
// transport, so a change of mode relaunches the direct identity (the only one the
// mode affects) to pick up the new policy.
func applyH3Mode(mode h3Mode) (previous h3Mode, relaunch bool) {
	connect.SetH3Enabled(mode == h3ModeOn)
	prev := h3Mode(h3AppliedMode.Swap(int32(mode)))
	if prev < 0 {
		return mode, false
	}
	return prev, prev != mode
}

// restartDirect relaunches the native [direct] identity so it rebuilds its
// platform transport under the current h3 mode. The reload pass starts it again
// once its goroutine has gone. Hot restart keeps its client credential, so this
// does not mint a new identity.
func (r *ProxyReloader) restartDirect() {
	r.cancelMapMu.Lock()
	cancel, running := r.cancelMap[directProxyKey]
	r.cancelMapMu.Unlock()
	if !running {
		return
	}
	r.mu.Lock()
	done := r.directDone
	r.mu.Unlock()
	cancel()
	if done != nil {
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			tlog("[h3] warn: the direct identity did not stop within 10s, relaunching anyway\n")
		}
	}
	triggerProxyReload()
}

func restartDirectIdentity() {
	if r := activeProxyReloader.Load(); r != nil {
		r.restartDirect()
	}
}
