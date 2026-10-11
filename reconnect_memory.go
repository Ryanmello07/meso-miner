package connect

import (
	"bufio"
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
)

const (
	// memoryPressureHostFreeShare: pressure when the kernel reports less than this
	// share of host memory available.
	memoryPressureHostFreeShare = 0.07
	// memoryPressureHeapShare: pressure when the Go heap is this close to the
	// soft memory limit, after which the runtime starts to thrash on GC.
	memoryPressureHeapShare = 0.92
)

// memoryPressureFrom decides from raw numbers whether memory is critically
// short. limit is the Go soft memory limit (0 when none is set); avail and total
// are the host's available and total memory (0 when unknown).
func memoryPressureFrom(heapInuse, limit, avail, total uint64) bool {
	if 0 < total && float64(avail) < memoryPressureHostFreeShare*float64(total) {
		return true
	}
	if 0 < limit && memoryPressureHeapShare*float64(limit) <= float64(heapInuse) {
		return true
	}
	return false
}

// readMemoryPressure samples the live process and host.
func readMemoryPressure() bool {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	limit := debug.SetMemoryLimit(-1)
	var softLimit uint64
	if 0 < limit && limit < (1<<62) {
		softLimit = uint64(limit)
	}
	avail, total := hostMemory()
	return memoryPressureFrom(ms.HeapInuse, softLimit, avail, total)
}

// hostMemory reads MemAvailable and MemTotal in bytes; zero when unreadable
// (non-Linux, restricted /proc), which reads as "unknown", never as pressure.
func hostMemory() (avail uint64, total uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemAvailable:":
			avail = kb << 10
		case "MemTotal:":
			total = kb << 10
		}
	}
	return avail, total
}
