package main

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func testH3Proxies(n int) []*connect.ProxySettings {
	out := make([]*connect.ProxySettings, n)
	for i := range out {
		out[i] = &connect.ProxySettings{Network: "tcp", Address: fmt.Sprintf("192.0.2.%d:1080", i+1), Auth: &proxy.Auth{User: "u", Password: "p"}}
	}
	return out
}

func gradedState(candidates []*connect.ProxySettings, scores map[int]float64) *ProxyState {
	state := &ProxyState{Proxies: map[string]ProxyEntry{}}
	for i, c := range candidates {
		if score, ok := scores[i]; ok {
			state.Proxies[c.Key()] = ProxyEntry{Graded: true, Score: score}
		}
	}
	return state
}

var _ = testing.Short
