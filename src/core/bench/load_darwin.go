package bench

import (
	"encoding/binary"
	"syscall"
)

// readLoadAverage reads vm.loadavg, a struct loadavg {fixpt_t ldavg[3]; long fscale}.
// syscall.Sysctl drops one trailing NUL, so the buffer is padded back to its size.
func readLoadAverage() *float64 {
	raw, err := syscall.Sysctl("vm.loadavg")
	if err != nil {
		return nil
	}
	b := make([]byte, 24)
	copy(b, raw)
	fscale := binary.LittleEndian.Uint64(b[16:24])
	if fscale == 0 {
		return nil
	}
	v := float64(binary.LittleEndian.Uint32(b[0:4])) / float64(fscale)
	return &v
}
