// The fleet command group (manifest, register, publish, bind, sign, status,
// revoke). Ported from the sn miner's fleet.go and friends; the family branch
// fills this file. Grammar and dispatch are final.
package main

import (
	"fmt"
	"os"

	"github.com/docopt/docopt-go"
)

func fleetCmd(opts docopt.Opts) {
	fmt.Println("fleet: not implemented yet")
	os.Exit(1)
}
