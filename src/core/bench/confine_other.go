//go:build !darwin

package bench

// confineCheck has no confinement on this platform yet; the verdict records that the check
// ran unconfined, so a reader can weigh the run.
func confineCheck(command, checkout string, allowed, denied []string) (string, bool) {
	return command, false
}
