package bench

import "fmt"

// loadAverage is the machine's one-minute load reading, indirected so a test can put it
// on a chosen side of a ceiling; nothing else replaces it.
var loadAverage = readLoadAverage

// loadControl refuses a run whose machine is already loaded, in the shape clockControl
// refuses a run whose node does not read the pin: a reading that fails its control
// refuses the run rather than becoming a column (p8 R39). Every column of the four live
// runs was measured at a load between 20 and 152 on an 8-processor machine, and their
// wall_ms figures are not comparable with each other (critique c8 F4).
func loadControl(load *float64, max float64) error {
	switch {
	case max <= 0:
		return nil
	case load == nil:
		return fmt.Errorf("load control: this platform has no load average to read against a ceiling of %.2f", max)
	case *load > max:
		return fmt.Errorf("load control: the one-minute load average reads %.2f, over the profile's max_load_at_start of %.2f", *load, max)
	}
	return nil
}
