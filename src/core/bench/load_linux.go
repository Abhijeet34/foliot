package bench

import (
	"os"
	"strconv"
	"strings"
)

// readLoadAverage reads the one-minute load from /proc/loadavg.
func readLoadAverage() *float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return nil
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return nil
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return nil
	}
	return &v
}
