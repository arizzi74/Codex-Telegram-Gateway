package speech

// x/sys/cpu checks AVX operating-system state but does not expose F16C.
// Query its independent CPUID bit as well before choosing that native build.
func hasF16C() bool
