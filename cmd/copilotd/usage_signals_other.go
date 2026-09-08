//go:build !unix

package main

// Windows pipe writes already return errors without Unix SIGPIPE handling.
func notifyUsagePipeErrors() func() { return func() {} }
