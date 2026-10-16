// The claim daemons (claim-daemon, claim-daemon-v2). Ported from the sn
// miner's claim_daemon.go and claim_v2_daemon.go; the family branch fills this
// file, sharing the durable plumbing commit. Grammar and dispatch are final.
package main

import (
	"fmt"
	"os"

	"github.com/docopt/docopt-go"
)

func claimDaemonCmd(opts docopt.Opts) {
	fmt.Println("claim-daemon: not implemented yet")
	os.Exit(1)
}

func claimDaemonV2Cmd(opts docopt.Opts) {
	fmt.Println("claim-daemon-v2: not implemented yet")
	os.Exit(1)
}
