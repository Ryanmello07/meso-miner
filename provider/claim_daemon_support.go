// The provider-lane glue the ported claim-daemon-v2 files need that sn's
// miner package kept elsewhere: sn's miner/fleet.go provided fleetOpt.
package main

import (
	"strings"

	"github.com/docopt/docopt-go"
)

// fleetOpt mirrors sn miner/fleet.go: the shared option reader of the miner
// command groups. The fleet family may reuse it or hoist it.
func fleetOpt(opts docopt.Opts, name string) string {
	v, _ := opts.String(name)
	return strings.TrimSpace(v)
}
