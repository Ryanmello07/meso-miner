//go:build bulkbench

package connect

// Bulk footprint harness for meso-miner engine decisions.
//
// It runs the connect transfer engine the way a bulk provider does: many
// independent clients (one per proxy identity) in one process. Nothing
// touches the network; clients are wired to each other with in-memory
// gateway transports, so the numbers isolate the engine's own CPU and memory
// cost and the same file runs unchanged against the meso engine and against
// upstream connect (only bulkDest differs, in a per-engine shim file).
//
// Run:
//   go test -tags bulkbench -run 'TestBulk' -count=1 -timeout 60m .
//
// Env:
//   BULK_N        idle client counts, comma separated (default 1,50,200)
//   BULK_SETTLE   settle time after creating clients (default 5s)
//   BULK_IDLE     idle CPU window (default 10s)
//   BULK_PAIRS    sender/receiver pairs for the transfer test (default 1,16,64)
//   BULK_MSGS     messages per pair (default 2000)
//   BULK_SIZE     message payload bytes (default 1024)
//   BULK_ENC      off | on (default: engine default settings)
//   BULK_OUT      append one JSON line per result to this file

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/protocol"
)

type bulkResult struct {
	Engine        string  `json:"engine"`
	Test          string  `json:"test"`
	Enc           string  `json:"enc"`
	N             int     `json:"n"`
	HeapPerClient float64 `json:"heap_kib_per_client,omitempty"`
	RssPerClient  float64 `json:"rss_kib_per_client,omitempty"`
	GoroutinesPer float64 `json:"goroutines_per_client,omitempty"`
	IdleCpuPct    float64 `json:"idle_cpu_pct_total,omitempty"`
	MBps          float64 `json:"mb_per_s,omitempty"`
	CpuMsPerMB    float64 `json:"cpu_ms_per_mb,omitempty"`
	PeakHeapMiB   float64 `json:"peak_heap_mib,omitempty"`
	PeakRssMiB    float64 `json:"peak_rss_mib,omitempty"`
	Seconds       float64 `json:"seconds,omitempty"`
	Delivered     int64   `json:"delivered,omitempty"`
}

func bulkEnv(key string, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func bulkInts(key string, def string) []int {
	var out []int
	for _, s := range strings.Split(bulkEnv(key, def), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

func bulkDuration(key string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return d
	}
	return def
}

func bulkRssKiB() float64 {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmRSS:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				v, _ := strconv.ParseFloat(f[1], 64)
				return v
			}
		}
	}
	return 0
}

func bulkCpu() time.Duration {
	var ru syscall.Rusage
	syscall.Getrusage(syscall.RUSAGE_SELF, &ru)
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano())
}

func bulkHeap() uint64 {
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.HeapInuse
}

func bulkEmit(t *testing.T, r bulkResult) {
	r.Engine = bulkEnv("BULK_ENGINE", bulkEngineName)
	r.Enc = bulkEnv("BULK_ENC", "default")
	b, _ := json.Marshal(r)
	t.Logf("BULK %s", b)
	if path := os.Getenv("BULK_OUT"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
}

func bulkSettings() *ClientSettings {
	settings := DefaultClientSettings()
	switch os.Getenv("BULK_ENC") {
	case "off":
		settings.EncryptionSettings.Mode = EncryptionModeOff
	case "on":
		settings.EncryptionSettings.Mode = EncryptionModeOpportunistic
		settings.EncryptionSettings.TlsTimeout = 60 * time.Second
	}
	return settings
}

var bulkProvideModes = map[protocol.ProvideMode]bool{
	protocol.ProvideMode_Network: true,
}

// TestBulkIdle measures the steady cost of N provider identities that are up
// but carry no traffic: heap, RSS and goroutines per client, and the total
// idle CPU the process burns.
func TestBulkIdle(t *testing.T) {
	for _, n := range bulkInts("BULK_N", "1,50,200") {
		t.Run(fmt.Sprintf("n=%d", n), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			time.Sleep(500 * time.Millisecond)
			heap0 := bulkHeap()
			rss0 := bulkRssKiB()
			g0 := runtime.NumGoroutine()

			// one shared sink drains every client's outbound route
			sink := make(chan []byte, 1024)
			go func() {
				for {
					select {
					case <-ctx.Done():
						return
					case <-sink:
					}
				}
			}()

			clients := make([]*Client, 0, n)
			for i := 0; i < n; i += 1 {
				c := NewClient(ctx, NewId(), NewNoContractClientOob(), bulkSettings())
				c.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{sink})
				c.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{make(chan []byte)})
				c.ContractManager().SetProvideModes(bulkProvideModes)
				clients = append(clients, c)
			}

			time.Sleep(bulkDuration("BULK_SETTLE", 5*time.Second))
			heap1 := bulkHeap()
			rss1 := bulkRssKiB()
			g1 := runtime.NumGoroutine()

			idle := bulkDuration("BULK_IDLE", 10*time.Second)
			cpu0 := bulkCpu()
			time.Sleep(idle)
			cpu1 := bulkCpu()

			bulkEmit(t, bulkResult{
				Test:          "idle",
				N:             n,
				HeapPerClient: float64(heap1-min(heap0, heap1)) / 1024 / float64(n),
				RssPerClient:  (rss1 - rss0) / float64(n),
				GoroutinesPer: float64(g1-g0) / float64(n),
				IdleCpuPct:    100 * float64(cpu1-cpu0) / float64(idle),
			})

			for _, c := range clients {
				c.Cancel()
			}
			cancel()
			time.Sleep(1 * time.Second)
		})
	}
}

// TestBulkTransfer pushes traffic through P independent sender/receiver pairs
// at once and reports throughput, CPU per MB delivered and peak memory.
func TestBulkTransfer(t *testing.T) {
	msgs := bulkInts("BULK_MSGS", "2000")[0]
	size := bulkInts("BULK_SIZE", "1024")[0]
	payload := strings.Repeat("x", size)

	for _, pairs := range bulkInts("BULK_PAIRS", "1,16,64") {
		t.Run(fmt.Sprintf("pairs=%d", pairs), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			var delivered atomic.Int64
			var acked atomic.Int64
			var wg sync.WaitGroup

			type pair struct {
				a, b       *Client
				aId, bId   Id
				aOut, bOut chan []byte
			}
			ps := make([]*pair, pairs)
			for i := range ps {
				p := &pair{
					aId:  NewId(),
					bId:  NewId(),
					aOut: make(chan []byte, 64),
					bOut: make(chan []byte, 64),
				}
				p.a = NewClient(ctx, p.aId, NewNoContractClientOob(), bulkSettings())
				p.b = NewClient(ctx, p.bId, NewNoContractClientOob(), bulkSettings())
				p.a.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{p.aOut})
				p.a.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{p.bOut})
				p.b.RouteManager().UpdateTransport(NewSendGatewayTransport(), []Route{p.bOut})
				p.b.RouteManager().UpdateTransport(NewReceiveGatewayTransport(), []Route{p.aOut})
				p.a.ContractManager().SetProvideModes(bulkProvideModes)
				p.b.ContractManager().SetProvideModes(bulkProvideModes)
				p.b.AddReceiveCallback(func(source TransferPath, frames []*protocol.Frame, peer Peer) {
					delivered.Add(int64(len(frames)))
				})
				for range 2 {
					err := p.a.ContractManager().HandleControlFrame(
						ContractKey{Destination: DestinationId(p.bId)},
						requireContractResult(
							protocol.ProvideMode_Network,
							p.b.ContractManager().RequireProvideSecretKey(protocol.ProvideMode_Network),
							p.aId,
							p.bId,
						),
					)
					if err != nil {
						t.Fatal(err)
					}
				}
				ps[i] = p
			}

			var peakHeap atomic.Uint64
			var peakRss atomic.Uint64
			stopSample := make(chan struct{})
			go func() {
				ticker := time.NewTicker(200 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case <-stopSample:
						return
					case <-ticker.C:
						var ms runtime.MemStats
						runtime.ReadMemStats(&ms)
						if ms.HeapInuse > peakHeap.Load() {
							peakHeap.Store(ms.HeapInuse)
						}
						if r := uint64(bulkRssKiB()); r > peakRss.Load() {
							peakRss.Store(r)
						}
					}
				}
			}()

			total := int64(pairs * msgs)
			start := time.Now()
			cpu0 := bulkCpu()
			for _, p := range ps {
				wg.Add(1)
				go func(p *pair) {
					defer wg.Done()
					for i := 0; i < msgs; i += 1 {
						frame, err := ToFrame(&protocol.SimpleMessage{Content: payload}, DefaultProtocolVersion)
						if err != nil {
							panic(err)
						}
						p.a.Send(frame, bulkDest(p.bId), func(err error) {
							acked.Add(1)
						})
					}
				}(p)
			}
			wg.Wait()

			deadline := time.Now().Add(bulkDuration("BULK_TIMEOUT", 10*time.Minute))
			for delivered.Load() < total && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			elapsed := time.Since(start)
			cpu1 := bulkCpu()
			close(stopSample)

			mb := float64(delivered.Load()) * float64(size) / 1e6
			r := bulkResult{
				Test:        "transfer",
				N:           pairs,
				Seconds:     elapsed.Seconds(),
				Delivered:   delivered.Load(),
				PeakHeapMiB: float64(peakHeap.Load()) / 1024 / 1024,
				PeakRssMiB:  float64(peakRss.Load()) / 1024,
			}
			if mb > 0 {
				r.MBps = mb / elapsed.Seconds()
				r.CpuMsPerMB = float64((cpu1 - cpu0).Milliseconds()) / mb
			}
			bulkEmit(t, r)
			if delivered.Load() < total {
				t.Errorf("delivered %d/%d", delivered.Load(), total)
			}

			for _, p := range ps {
				p.a.Cancel()
				p.b.Cancel()
			}
			cancel()
			time.Sleep(1 * time.Second)
		})
	}
}
