# Go and CGO-free builds

Context: workers run on Linux and macOS, amd64 and arm64.

Decision: use one Go module, standard-library HTTP and explicit protocol
packages. Build with CGO_ENABLED=0, -trimpath and -ldflags '-s -w'.

Consequences: Linux executables are statically linked and stripped. Darwin
executables use the operating system's normal system interface without CGO.
Cross-build both worker and local CLI helpers for all four targets.
