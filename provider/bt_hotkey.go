// The wallet hotkey commands (challenge, set, status). Ported from the sn
// miner's hotkey_wallet_cmd.go; the family branch fills this file. The docopt
// grammar and dispatch in main.go are final: only this file changes.
package main

import (
	"fmt"
	"os"

	"github.com/docopt/docopt-go"
)

func hotkeyWalletCmd(opts docopt.Opts) {
	fmt.Println("wallet hotkey: not implemented yet")
	os.Exit(1)
}
