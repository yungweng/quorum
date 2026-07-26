package runner

import (
	"encoding/binary"

	"golang.org/x/sys/unix"
)

// LoadAvg1 returns the one minute load average.
//
// Darwin exposes it as struct loadavg { fixpt_t ldavg[3]; long fscale; }, which
// on arm64 is three uint32 followed by four bytes of padding and a 64 bit
// scale. Verified against uptime.
func LoadAvg1() (float64, bool) {
	raw, err := unix.SysctlRaw("vm.loadavg")
	if err != nil || len(raw) < 24 {
		return 0, false
	}
	ldavg := binary.LittleEndian.Uint32(raw[0:4])
	fscale := binary.LittleEndian.Uint64(raw[16:24])
	if fscale == 0 {
		return 0, false
	}
	return float64(ldavg) / float64(fscale), true
}
