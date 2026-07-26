package runner

import (
	"os"
	"strconv"
	"strings"
)

// LoadAvg1 returns the one minute load average from /proc/loadavg.
func LoadAvg1() (float64, bool) {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
