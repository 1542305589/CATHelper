//go:build !linux

package daemon

// freeDiskBytes is unknown outside Linux; the caller proceeds without the
// free-space check so the copy path remains testable on Windows.
func freeDiskBytes(string) (uint64, bool) {
	return 0, false
}
