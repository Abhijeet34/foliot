package bench

import "path/filepath"

// RealPath resolves path to its real, symlink-free form. An OS sandbox that matches
// real paths (Seatbelt on macOS) checks the path the kernel resolves, not the one a
// caller wrote, so a deny or allow rule built from an unresolved path such as
// /tmp/... (a symlink to /private/tmp/... on stock macOS) silently never matches. A
// path that does not exist yet resolves through its nearest existing ancestor, with
// the missing remainder rejoined unresolved.
func RealPath(path string) string {
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	dir := filepath.Dir(path)
	if dir == path {
		return path
	}
	return filepath.Join(RealPath(dir), filepath.Base(path))
}
