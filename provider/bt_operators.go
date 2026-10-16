// The operators surface: `provider operators` and `provider provide
// --all-operators`. Ported from the sn miner; the family branch fills this
// file. Grammar and dispatch are final.
package main

import (
	"fmt"
	"os"

	"github.com/docopt/docopt-go"
)

func operatorsCmd(opts docopt.Opts) {
	fmt.Println("operators: not implemented yet")
	os.Exit(1)
}

func provideAllOperatorsCmd(opts docopt.Opts) {
	fmt.Println("provide --all-operators: not implemented yet")
	os.Exit(1)
}
