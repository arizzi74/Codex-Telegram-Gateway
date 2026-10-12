//go:build !amd64

package speech

func hasF16C() bool { return false }
