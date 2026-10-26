//go:build !unix

package spool

import "os"

// lock is a no-op where flock does not exist: there, only one gateway may
// use a spool directory (README).
func lock(*os.File, bool) error { return nil }
