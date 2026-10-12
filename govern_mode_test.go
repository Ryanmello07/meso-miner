package connect

import "testing"

func TestParseGovernMode_DefaultsToShadowOnThisLine(t *testing.T) {
	for raw, want := range map[string]governMode{
		"": governShadow, "   ": governShadow, "garbage": governShadow, "shadow": governShadow,
		"enforce": governEnforce, "ENFORCE": governEnforce, "on": governEnforce, "1": governEnforce, "true": governEnforce,
		"off": governOff, "0": governOff, "false": governOff, "no": governOff,
	} {
		if got := parseGovernMode(raw); got != want {
			t.Errorf("parseGovernMode(%q) = %v, want %v", raw, got, want)
		}
	}
}
