// cli_command_refs_test.go — docs and comments that tell someone to run
// `provider <cmd>` must name a command the CLI has. A removed or renamed
// command left in the docs sends the reader to something that does not exist.
//
// Ported from sn's miner/cli_command_refs_test.go (TestDocumentedCliCommandsExist).
// The sn original also checks the snclaim binary and a nested fleet namespace;
// neither exists on this line (head binding here is the top-level
// `provider bind-head` / `provider unbind-head`), so only the provider's own
// command set is checked.
package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestDocumentedCliCommandsExist(t *testing.T) {
	commands := usageCommands(mainUsage(), "provider")
	if len(commands) == 0 {
		t.Fatal("no provider commands found in the usage")
	}
	// `provider help` is a real alias handled before docopt is consulted (see
	// main), so it is deliberately absent from the usage text. Everything else
	// the docs name has to be a usage command.
	for _, alias := range []string{"help"} {
		commands[alias] = true
	}

	reference := regexp.MustCompile("`provider ([a-z][a-z0-9_-]*)")
	root := ".."
	skip := map[string]bool{
		".git":                          true,
		"third_party":                   true,
		"node_modules":                  true,
		filepath.Join("docs", "parked"): true,
	}

	var missing []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if entry.IsDir() {
			if skip[rel] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") && !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, match := range reference.FindAllStringSubmatch(string(b), -1) {
			if !commands[match[1]] {
				missing = append(missing, rel+": provider "+match[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("documented commands missing from the CLI usage:\n  %s", strings.Join(missing, "\n  "))
	}
}

// usageCommands returns the first word after the binary name on each docopt
// usage line.
func usageCommands(usage string, binary string) map[string]bool {
	line := regexp.MustCompile(`(?m)^\s*` + binary + ` ([a-z][a-z0-9_-]*)`)
	out := map[string]bool{}
	for _, match := range line.FindAllStringSubmatch(usage, -1) {
		out[match[1]] = true
	}
	return out
}
