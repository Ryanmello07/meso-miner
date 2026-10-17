// The operators surface: `provider operators`, `provider provide
// --all-operators` and `provider auth --operator=<domain>`. Ported from the sn
// miner's operators_cmd.go (upstream 6608298e), adapted to the v33 provider:
//
//   - The state directory is ~/.urnetwork: this binary keys every state path
//     off HOME and has no URNETWORK_STATE_DIR, so each operator's directory
//     below operators/ doubles as its child provider's home (see
//     bt_operators_supervisor.go, operatorChildEnv).
//   - `auth --operator` writes the operator's jwt from a hotkey sign-in, an
//     auth code or a username and password, against the operator's api url
//     from the list. It never creates a hotkey seed: a missing or unsafe seed
//     file is an error naming its path.
//   - --api_url stays in the --all-operators pattern so an explicit value is
//     refused by name, as before.
//
// Every operator keeps its own state directory, operatorlist.DomainStateDir
// below the provider state directory, and the list is cached as operators.yml
// in the provider state directory, so these commands work while ur.xyz is
// unreachable.
package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/docopt/docopt-go"
	"golang.org/x/term"

	"github.com/urnetwork/connect"

	"github.com/urfoundation/sn/hotkeyauth"
	"github.com/urfoundation/sn/operatorlist"
)

const defaultOperatorsRefresh = time.Hour

// operatorStateSubdirectory is where one operator's provider state lives
// inside the operator's directory: the directory itself is the child
// provider's home (HOME), and the v33 provider keeps its state in
// ~/.urnetwork.
const operatorStateSubdirectory = ".urnetwork"

// operatorBaseStateDir is the provider state directory ~/.urnetwork that
// every other v33 state path (jwt, .provider.key, .client_jwts.json,
// network.json, proxies) also lives in. mustStateDir is that same directory;
// it only answers "" when the home directory is unavailable.
func operatorBaseStateDir() (string, error) {
	if base := mustStateDir(); base != "" {
		return base, nil
	}
	return "", errors.New("cannot determine the provider state directory (~/.urnetwork)")
}

// operatorDirectory is one operator's directory below the provider state
// directory. It is the child provider's home, and its .urnetwork below is
// the child's state directory.
func operatorDirectory(base string, domain string) string {
	return operatorlist.DomainStateDir(base, domain)
}

// operatorJwtPath is one operator's network jwt: the jwt written for that
// operator, and the credential the supervisor and `provider operators`
// check. Also used by the wallet hotkey commands.
func operatorJwtPath(base string, domain string) string {
	return filepath.Join(operatorDirectory(base, domain), operatorStateSubdirectory, "jwt")
}

func operatorListCachePath(base string) string {
	return filepath.Join(base, "operators.yml")
}

// readOperatorJwt reads one operator's jwt with the same contract as the sn's
// clientauth.ReadToken: the file is trimmed, and an empty one is an error.
// It is repeated here because sn/clientauth imports github.com/urnetwork/sdk,
// which this module does not carry; nothing else in clientauth is used by
// this family.
func readOperatorJwt(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(b))
	if token == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return token, nil
}

// One fetch of the list, or the cached copy when the fetch fails.
func loadOperatorList(ctx context.Context, opts docopt.Opts, base string) (*operatorlist.Snapshot, error) {
	operatorsUrl, _ := opts.String("--operators-url")
	return operatorlist.Load(ctx, operatorlist.RefreshSettings{Url: operatorsUrl, CachePath: operatorListCachePath(base)})
}

// The frozen grammar leaves --api_url in the --all-operators pattern; it has
// no meaning there, because every operator's urls come from the operator
// list (the sn refuses it the same way). An explicit value is refused by
// name rather than silently ignored.
func allOperatorsArgsError(opts docopt.Opts) error {
	if apiUrl, err := opts.String("--api_url"); err == nil && apiUrl != "" {
		return fmt.Errorf("provide --all-operators excludes --api_url=%s: each operator's urls come from the operator list", apiUrl)
	}
	return nil
}

// Reads the --all-operators options. The hotkey is loaded here, so a bad seed
// file stops startup before any child runs.
func newOperatorSupervisorSettings(opts docopt.Opts) (operatorSupervisorSettings, operatorlist.RefreshSettings, error) {
	base, err := operatorBaseStateDir()
	if err != nil {
		return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, err
	}
	refresh := defaultOperatorsRefresh
	if raw, err := opts.String("--operators-refresh"); err == nil {
		refresh, err = time.ParseDuration(raw)
		if err != nil || refresh < time.Minute {
			return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, fmt.Errorf("--operators-refresh=%s must be a Go duration of at least 1m", raw)
		}
	}
	operatorsUrl, _ := opts.String("--operators-url")
	settings := operatorSupervisorSettings{baseStateDir: base}
	// as for one operator, a nonpositive limit is no limit
	if raw, err := opts.String("--max-memory"); err == nil {
		settings.maxMemory, err = connect.ParseByteCount(raw)
		if err != nil {
			return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, fmt.Errorf("--max-memory=%s is not a byte count such as 2gib", raw)
		}
	}
	if settings.port, err = opts.Int("--port"); err != nil {
		return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, fmt.Errorf("--port: %w", err)
	}
	settings.verbosity, _ = opts["-v"].(int)
	settings.allowClientRegistration, _ = opts.Bool("--allow-client-registration")
	settings.autoRegister, _ = opts.Bool("--auto-register")
	if seedPath, err := opts.String("--hotkey_seed_file"); err == nil && seedPath != "" {
		if settings.hotkey, err = loadOperatorHotkey(seedPath); err != nil {
			return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, err
		}
	}
	if settings.autoRegister && settings.hotkey == nil {
		return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, errors.New("--auto-register needs --hotkey_seed_file=<path>")
	}
	if settings.executable, err = os.Executable(); err != nil {
		return operatorSupervisorSettings{}, operatorlist.RefreshSettings{}, err
	}
	listSettings := operatorlist.RefreshSettings{Url: operatorsUrl, Interval: refresh, CachePath: operatorListCachePath(base)}
	return settings, listSettings, nil
}

// "provider provide --all-operators".
func provideAllOperatorsCmd(opts docopt.Opts) {
	if err := allOperatorsArgsError(opts); err != nil {
		fmt.Fprintf(os.Stderr, "provide --all-operators: %v\n", err)
		os.Exit(1)
	}
	settings, listSettings, err := newOperatorSupervisorSettings(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provide --all-operators: %v\n", err)
		os.Exit(1)
	}
	supervisor := newOperatorSupervisor(settings, defaultOperatorSupervisorHooks(), os.Stderr)
	if settings.hotkey != nil && !settings.autoRegister {
		// the hotkey only signs in with --auto-register on this line; Stage A
		// has no hotkey wallet upkeep to use it for otherwise
		fmt.Fprintln(os.Stderr, "provide --all-operators: the hotkey is loaded but --auto-register is not set; it is not used for operator sign-in")
	}
	// port 0 (the default) serves nothing
	status, err := supervisor.serveStatus(settings.port)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provide --all-operators: status server: %v\n", err)
		os.Exit(1)
	}
	// subscribed before any child exists, so every stop signal reaches the
	// children through the supervisor
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	ctx := context.Background()
	refresher, err := operatorlist.NewRefresher(ctx, listSettings)
	if err != nil {
		fmt.Fprintf(os.Stderr, "provide --all-operators: %v\n", err)
		os.Exit(1)
	}
	supervisor.Run(ctx, refresher, signals)
	signal.Stop(signals)
	refresher.Close()
	if status != nil {
		_ = status.Close()
	}
}

// "provider auth --operator=<domain>".
func operatorAuthCmd(opts docopt.Opts) {
	event := connect.NewEventWithContext(context.Background())
	event.SetOnSignals(syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(event.Ctx())
	err := operatorAuth(ctx, opts, os.Stdout)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "auth --operator: %v\n", err)
		os.Exit(1)
	}
}

// confirmOperatorJwtOverwrite asks before replacing an existing jwt unless -f.
// The prompt names the path only.
func confirmOperatorJwtOverwrite(opts docopt.Opts, jwtPath string) bool {
	if _, err := os.Stat(jwtPath); errors.Is(err, os.ErrNotExist) {
		return true
	}
	if force, _ := opts.Bool("-f"); force {
		return true
	}
	fmt.Printf("%s exists. Overwrite? [yN]\n", jwtPath)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line)) == "y"
}

// Writes one listed operator's network jwt, from the logins auth uses against
// the operator's api url, or from a hotkey sign-in.
func operatorAuth(ctx context.Context, opts docopt.Opts, out io.Writer) error {
	domain, _ := opts.String("--operator")
	base, err := operatorBaseStateDir()
	if err != nil {
		return err
	}
	snapshot, err := loadOperatorList(ctx, opts, base)
	if err != nil {
		return err
	}
	operator, ok := snapshot.List.Operator(domain)
	if !ok {
		return fmt.Errorf("operator %q is not in the operator list (%s); provider operators prints the list", domain, snapshot.Sha256)
	}
	// the domain was validated by the list, so it is safe as a path
	jwtPath := operatorJwtPath(base, operator.Domain)
	if !confirmOperatorJwtOverwrite(opts, jwtPath) {
		return nil
	}
	var byJwt string
	if seedPath, err := opts.String("--hotkey_seed_file"); err == nil && seedPath != "" {
		hotkey, err := loadOperatorHotkey(seedPath)
		if err != nil {
			return err
		}
		network, err := hotkeyauth.SignIn(ctx, hotkeyauth.Settings{ApiUrl: operator.ApiUrl, Hotkey: hotkey})
		if err != nil {
			return err
		}
		if network.Created {
			fmt.Fprintf(out, "Created the hotkey's network on %s\n", operator.Domain)
		}
		byJwt = network.ByJwt
	} else if byJwt, err = operatorNetworkLogin(ctx, opts, operator.ApiUrl); err != nil {
		return err
	}
	if byJwt == "" {
		return fmt.Errorf("operator %s returned no jwt", operator.Domain)
	}
	if err := persistOperatorJwt(jwtPath, byJwt); err != nil {
		return err
	}
	fmt.Fprintf(out, "Jwt written to %s\n", jwtPath)
	if _, err := os.Stat(filepath.Join(filepath.Dir(jwtPath), ".provider.key")); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "This operator has no provider client yet; its first run needs: provider provide --all-operators --allow-client-registration")
	}
	return nil
}

// operatorNetworkLogin is the auth code or username and password login the
// single-operator auth command uses, against one operator's api url, without
// writing anything: the caller writes the operator's jwt.
func operatorNetworkLogin(ctx context.Context, opts docopt.Opts, apiUrl string) (string, error) {
	clientStrategy := connect.NewClientStrategyWithDefaults(ctx)
	api := connect.NewBringYourApi(ctx, clientStrategy, apiUrl)
	if userAuth, err := opts.String("--user_auth"); err == nil && userAuth != "" {
		password, _ := opts.String("--password")
		if password == "" {
			fmt.Print("Enter password: ")
			passwordBytes, err := termReadPassword()
			if err != nil {
				return "", err
			}
			password = string(passwordBytes)
			fmt.Printf("\n")
		}
		loginCallback, loginChannel := connect.NewBlockingApiCallback[*connect.AuthLoginWithPasswordResult](ctx)
		api.AuthLoginWithPassword(&connect.AuthLoginWithPasswordArgs{UserAuth: userAuth, Password: password}, loginCallback)
		var loginResult connect.ApiCallbackResult[*connect.AuthLoginWithPasswordResult]
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case loginResult = <-loginChannel:
		}
		if loginResult.Error != nil {
			return "", loginResult.Error
		}
		if loginResult.Result == nil || loginResult.Result.Error != nil {
			message := "authentication failed"
			if loginResult.Result != nil && loginResult.Result.Error != nil && loginResult.Result.Error.Message != "" {
				message = "authentication failed: " + loginResult.Result.Error.Message
			}
			return "", errors.New(message)
		}
		if loginResult.Result.VerificationRequired != nil {
			return "", fmt.Errorf("verification required for %s; complete account setup via the app or web first", loginResult.Result.VerificationRequired.UserAuth)
		}
		if loginResult.Result.Network == nil {
			return "", errors.New("authentication returned no network")
		}
		return loginResult.Result.Network.ByJwt, nil
	}
	authCode, _ := opts.String("<auth_code>")
	if authCode == "" {
		fmt.Print("Enter auth code: ")
		authCodeBytes, err := termReadPassword()
		if err != nil {
			line, readErr := bufio.NewReader(os.Stdin).ReadString('\n')
			if line == "" && readErr != nil {
				return "", fmt.Errorf("could not read the auth code from stdin: %w", readErr)
			}
			authCodeBytes = []byte(line)
		}
		authCode = strings.TrimSpace(string(authCodeBytes))
		fmt.Printf("\n")
	}
	authCodeCallback, authCodeChannel := connect.NewBlockingApiCallback[*connect.AuthCodeLoginResult](ctx)
	api.AuthCodeLogin(&connect.AuthCodeLoginArgs{AuthCode: authCode}, authCodeCallback)
	var authCodeResult connect.ApiCallbackResult[*connect.AuthCodeLoginResult]
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case authCodeResult = <-authCodeChannel:
	}
	if authCodeResult.Error != nil {
		return "", authCodeResult.Error
	}
	if authCodeResult.Result == nil || authCodeResult.Result.Error != nil {
		message := "authentication code rejected"
		if authCodeResult.Result != nil && authCodeResult.Result.Error != nil && authCodeResult.Result.Error.Message != "" {
			message = "authentication code rejected: " + authCodeResult.Result.Error.Message
		}
		return "", errors.New(message)
	}
	return authCodeResult.Result.ByJwt, nil
}

// persistOperatorJwt writes one operator's network token owner-only, below the
// operator's .urnetwork state directory.
func persistOperatorJwt(jwtPath string, token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return errors.New("refusing to persist an empty network token")
	}
	if err := os.MkdirAll(filepath.Dir(jwtPath), 0700); err != nil {
		return err
	}
	return atomicWriteFile(jwtPath, []byte(token), 0600)
}

// termReadPassword reads one password or auth code from the terminal, with the
// echo suppressed.
func termReadPassword() ([]byte, error) {
	return term.ReadPassword(int(syscall.Stdin))
}

// "provider operators".
func operatorsCmd(opts docopt.Opts) {
	if err := printOperators(context.Background(), opts, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "operators: %v\n", err)
		os.Exit(1)
	}
}

// The list's source and digest, then each listed operator with its credential
// state and urls.
func printOperators(ctx context.Context, opts docopt.Opts, out io.Writer) error {
	base, err := operatorBaseStateDir()
	if err != nil {
		return err
	}
	snapshot, err := loadOperatorList(ctx, opts, base)
	if err != nil {
		return err
	}
	if snapshot.Cached {
		fmt.Fprintf(out, "source: cached %s, fetched %s\n", operatorListCachePath(base), snapshot.FetchedAt.UTC().Format(time.RFC3339))
	} else {
		operatorsUrl, _ := opts.String("--operators-url")
		if operatorsUrl == "" {
			operatorsUrl = operatorlist.DefaultUrl
		}
		fmt.Fprintf(out, "source: fetched %s\n", operatorsUrl)
	}
	fmt.Fprintf(out, "digest: %s\n", snapshot.Sha256)
	table := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	awaiting := false
	for _, operator := range snapshot.List.Operators {
		state := "jwt present"
		if _, err := readOperatorJwt(operatorJwtPath(base, operator.Domain)); err != nil {
			state = "awaiting auth"
			awaiting = true
		}
		fmt.Fprintf(table, "%s\t%s\t%s\t%s\n", operator.Domain, state, operator.ApiUrl, operator.ConnectUrl)
	}
	if err := table.Flush(); err != nil {
		return err
	}
	if awaiting {
		fmt.Fprintln(out, "authenticate an operator awaiting auth with: provider auth --operator=<domain>")
	}
	return nil
}
