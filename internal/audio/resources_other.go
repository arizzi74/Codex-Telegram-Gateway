//go:build !linux && !darwin

package audio

func platformResources() (int, int64, int64) { return 0, 0, 0 }
