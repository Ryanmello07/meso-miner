package connect

// HTTPUpgradeError extracted from upstream http_upgrade.go at 20b29f79 for
// the credential-refusal leaf's error classification. Merge back when the
// framing layer ports; the rest of http_upgrade.go is not ported yet.

import "fmt"

// HTTPUpgradeError contains only bounded protocol classifications, never a URL,
// credential, response body, or peer-supplied status text.
type HTTPUpgradeError struct {
	StatusCode int
	Reason     string
	Terminal   bool
}

func (e *HTTPUpgradeError) Error() string {
	return fmt.Sprintf("HTTP upgrade: %s (status %d)", e.Reason, e.StatusCode)
}
