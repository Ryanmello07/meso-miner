package main

// fleet_rpc.go — the read-only JSON-RPC eth_call view path behind
// `provider fleet status/bind/revoke` on the testnet compatibility path (the
// mainnet path uses the EVM client from sn/miner/onchain directly).
//
// Port of the sn miner's sn_rpc.go read chain (the read-only generation with
// bounded retries, response admission and structured retry classification),
// plus sn_rpc_retry.go and sn_read_retry_cause.go in full. The three names
// that already exist in this package — ethRpcHexResult (provider/sn_rpc.go,
// the claim read path) and its parseEthHexQuantity/parseEthHexBytes helpers —
// are reused, not redeclared; the retry entry points below are the
// upstream-named additions.
//
// Family note (#3693 split): ethRpcHexView, retryEthRpcRead,
// retryableEthRpcError and the cause-budget helpers are shared with the
// claim family's sn_rpc port; if both branches carry them, keep one copy at
// merge time.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/ethereum/go-ethereum/common/hexutil"

	"github.com/urfoundation/sn/protocol"
)

// Bound each individual request and the complete decoded response body.
const (
	ethRpcResponseLimit = 1024 * 1024
)

// Read-only calls recover transient transport failures within one finite
// budget. Both allowed methods return canonical 0x-prefixed hex strings.
func ethRpcHexResultWithClient(ctx context.Context, client *http.Client, rpcUrl string, method string, params []any) (string, error) {
	return ethRpcHexResultWithRetry(ctx, client, rpcUrl, method, params, ethRpcRetryHooks{})
}

// Authenticate the configured chain before asking it for contract state. A
// wrong-chain node never gains a view request or transient failover authority.
func ethRpcHexView(ctx context.Context, rpcUrl string, expectedChainId uint64, params []any) ([]byte, error) {
	chainIdHex, err := ethRpcHexResultWithClient(ctx, http.DefaultClient, rpcUrl, "eth_chainId", []any{})
	if err != nil {
		return nil, err
	}
	chainId, err := parseEthHexQuantity(chainIdHex)
	if err != nil {
		return nil, err
	}
	if chainId != expectedChainId {
		return nil, fmt.Errorf("%s chain id %d differs from expected %d", rpcUrl, chainId, expectedChainId)
	}
	result, err := ethRpcHexResultWithClient(ctx, http.DefaultClient, rpcUrl, "eth_call", params)
	if err != nil {
		return nil, err
	}
	return parseEthHexBytes(result)
}

// Marshal once before retry admission so every attempt retains the same
// method, selector, contract and calldata, even if caller data later changes.
func ethRpcHexResultWithRetry(ctx context.Context, client *http.Client, rpcUrl string, method string, params []any, hooks ethRpcRetryHooks) (string, error) {
	if ctx == nil || client == nil || (method != "eth_chainId" && method != "eth_call") {
		return "", errors.New("EVM read-only RPC owner or method is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	requestBodyBytes, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return "", err
	}
	return retryEthRpcRead(ctx, hooks, func(attemptCtx context.Context) (string, error) {
		return ethRpcHexRequest(attemptCtx, client, rpcUrl, method, requestBodyBytes)
	})
}

// Each attempt owns its complete response and closes it before retrying or
// returning any result. Only physical transport errors receive retry origin.
func ethRpcHexRequest(ctx context.Context, client *http.Client, rpcUrl string, method string, requestBodyBytes []byte) (string, error) {
	requestCtx, requestCancel := context.WithTimeout(ctx, ethRpcTimeout)
	defer requestCancel()
	request, err := http.NewRequestWithContext(requestCtx, "POST", rpcUrl, bytes.NewReader(requestBodyBytes))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	// A configured endpoint is read authority; redirects must not substitute
	// another node even when it returns a plausible chain id and result.
	requestClient := *client
	requestClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := requestClient.Do(request)
	if err != nil {
		return "", &ethRpcTransportError{cause: err}
	}
	// Close belongs to the same physical response as Read. Its transient
	// failures can retry, while every joined hard cause still defeats recovery.
	closeResponse := func() error {
		if err := response.Body.Close(); err != nil {
			return &ethRpcTransportError{cause: err}
		}
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return "", errors.Join(&ethRpcStatusError{method: method, status: response.StatusCode}, closeResponse(), requestCtx.Err())
	}
	if response.ContentLength > ethRpcResponseLimit {
		return "", errors.Join(fmt.Errorf("%s: RPC response exceeds %d bytes", method, ethRpcResponseLimit), closeResponse(), requestCtx.Err())
	}
	responseBodyBytes, readErr := io.ReadAll(io.LimitReader(response.Body, ethRpcResponseLimit+1))
	if readErr != nil {
		readErr = &ethRpcTransportError{cause: readErr}
	}
	if len(responseBodyBytes) > ethRpcResponseLimit {
		readErr = errors.Join(readErr, fmt.Errorf("%s: RPC response exceeds %d bytes", method, ethRpcResponseLimit))
	}
	responseErr := errors.Join(readErr, closeResponse(), requestCtx.Err())
	if readErr != nil || responseErr != nil && !retryableEthRpcError(responseErr, false) {
		return "", responseErr
	}
	// A complete contradictory response stays hard even when close also
	// reports a retryable transport failure. No refused result is returned.
	refuse := func(err error) (string, error) { return "", errors.Join(err, responseErr) }
	if err := protocol.ValidateUniqueJsonKeys(responseBodyBytes); err != nil {
		return refuse(fmt.Errorf("%s: malformed JSON-RPC response: %v", method, err))
	}
	var rpcResponse struct {
		Version string          `json:"jsonrpc"`
		Id      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(responseBodyBytes, &rpcResponse); err != nil {
		return refuse(fmt.Errorf("%s: bad json-rpc response: %w", method, err))
	}
	if rpcResponse.Version != "2.0" || !bytes.Equal(rpcResponse.Id, []byte("1")) || (len(rpcResponse.Result) == 0) == (len(rpcResponse.Error) == 0) {
		return refuse(fmt.Errorf("%s: JSON-RPC response identity or result/error envelope differs from the request", method))
	}
	if len(rpcResponse.Error) != 0 {
		var rpcError *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rpcResponse.Error, &rpcError); err != nil || rpcError == nil {
			return refuse(fmt.Errorf("%s: malformed JSON-RPC error", method))
		}
		return refuse(fmt.Errorf("%s: rpc error %d: %s", method, rpcError.Code, rpcError.Message))
	}
	var hexResult string
	if err := json.Unmarshal(rpcResponse.Result, &hexResult); err != nil {
		return refuse(fmt.Errorf("%s: non-string result", method))
	}
	// The package's ethRpcHexResult path owns lenient quantity/data parsing;
	// this generation requires the canonical 0x prefix before handing bytes to
	// it, so a prefixless result stays refused exactly as upstream refuses it.
	if !strings.HasPrefix(hexResult, "0x") {
		err = hexutil.ErrMissingPrefix
	} else if method == "eth_chainId" {
		_, err = parseEthHexQuantity(hexResult)
	} else {
		_, err = parseEthHexBytes(hexResult)
	}
	if err != nil {
		return refuse(fmt.Errorf("%s: malformed hexadecimal result: %w", method, err))
	}
	if err := errors.Join(responseErr, requestCtx.Err()); err != nil {
		return "", err
	}
	return hexResult, nil
}

const (
	ethRpcOperationTimeout = 300 * time.Second
	ethRpcRetryDelay       = 2 * time.Second
)

// Per-operation hooks allow deadline and cancellation tests without clock
// races. Production supplies neither hook; there is no mutable global state.
type ethRpcRetryHooks struct {
	withTimeout func(context.Context, time.Duration) (context.Context, context.CancelFunc)
	wait        func(context.Context, time.Duration) error
}

// HTTP status carries structured retry eligibility; diagnostic words do not.
type ethRpcStatusError struct {
	method string
	status int
}

// Retain the method and status for callers without inspecting response bodies.
func (self *ethRpcStatusError) Error() string {
	return fmt.Sprintf("%s: http %d", self.method, self.status)
}

// Only the request transport and bounded body reader may assign this origin.
type ethRpcTransportError struct {
	cause error
}

// Preserve the original diagnostic and structured cause.
func (self *ethRpcTransportError) Error() string { return self.cause.Error() }

// Expose timeout and cancellation while retaining transport provenance.
func (self *ethRpcTransportError) Unwrap() error { return self.cause }

// The operation has time to recover a minute-long outage, while each attempt
// retains its shorter request timeout. Cancellation always defeats recovery.
func retryEthRpcRead(ctx context.Context, hooks ethRpcRetryHooks, read func(context.Context) (string, error)) (string, error) {
	withTimeout := hooks.withTimeout
	if withTimeout == nil {
		withTimeout = context.WithTimeout
	}
	ctx, cancel := withTimeout(ctx, ethRpcOperationTimeout)
	defer cancel()
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return "", errors.Join(lastErr, err)
		}
		value, err := read(ctx)
		if ownerErr := ctx.Err(); ownerErr != nil {
			return "", errors.Join(err, ownerErr)
		}
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryableEthRpcError(err, false) {
			return "", err
		}
		delay := ethRpcRetryDelay + time.Duration(rand.Int64N(int64(ethRpcRetryDelay/2)))
		wait := hooks.wait
		if wait == nil {
			wait = func(ctx context.Context, delay time.Duration) error {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(delay):
					return nil
				}
			}
		}
		if err := wait(ctx, delay); err != nil {
			return "", errors.Join(lastErr, err)
		}
	}
}

// Every joined cause must permit retry. A timeout cannot hide malformed JSON,
// an identity conflict, a contract refusal, or a response-close integrity error.
func retryableEthRpcError(err error, transportOrigin bool) bool {
	budget := &minerReadCauseBudget{remaining: minerReadCauseMaximumNodes}
	return retryableEthRpcCause(err, transportOrigin, 0, budget)
}

// A delegated claim transport subtree consumes the same traversal allowance.
func retryableEthRpcCause(err error, transportOrigin bool, depth int, budget *minerReadCauseBudget) bool {
	if !budget.admit(err, depth) || err == context.Canceled {
		return false
	}
	switch err.(type) {
	case *os.PathError, *os.LinkError:
		return false
	}
	if err == context.DeadlineExceeded || err == net.ErrClosed || err == syscall.ECONNRESET || err == syscall.ECONNREFUSED || err == syscall.EPIPE || err == syscall.ETIMEDOUT || err == syscall.ENETUNREACH || err == syscall.EHOSTUNREACH {
		return true
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return transportOrigin
	}
	switch cause := err.(type) {
	case *ethRpcStatusError:
		return cause.status == http.StatusTooManyRequests || cause.status >= 500 && cause.status <= 599
	case *ethRpcTransportError:
		return retryableEthRpcCause(cause.cause, true, depth+1, budget)
	case *url.Error:
		return retryableEthRpcCause(cause.Err, true, depth+1, budget)
	case *net.OpError:
		return retryableEthRpcCause(cause.Err, true, depth+1, budget)
	case *net.DNSError:
		// Explicit absence stays hard. A wrapped cause retains its own
		// verdict; availability flags decide only a leaf without a child.
		if cause.IsNotFound {
			return false
		}
		if cause.UnwrapErr != nil {
			return retryableEthRpcCause(cause.UnwrapErr, transportOrigin, depth+1, budget)
		}
		return cause.IsTimeout || cause.IsTemporary
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 || len(causes) > budget.remaining {
			return false
		}
		for _, cause := range causes {
			if !retryableEthRpcCause(cause, transportOrigin, depth+1, budget) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryableEthRpcCause(wrapped.Unwrap(), transportOrigin, depth+1, budget)
	}
	if networkErr, ok := err.(net.Error); ok {
		return networkErr.Timeout() || networkErr.Temporary()
	}
	return false
}

const (
	minerReadCauseMaximumDepth = 32
	minerReadCauseMaximumNodes = 128
)

// Policies share traversal bounds without sharing status authority.
type minerReadCauseBudget struct {
	remaining int
}

// Reject nil receivers before invoking custom unwrap or network methods.
func (self *minerReadCauseBudget) admit(err error, depth int) bool {
	self.remaining--
	if err == nil || self.remaining < 0 || depth > minerReadCauseMaximumDepth {
		return false
	}
	value := reflect.ValueOf(err)
	switch value.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return !value.IsNil()
	default:
		return true
	}
}
