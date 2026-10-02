// Package version holds the build version (overridden with -ldflags at build time).
package version

// Version is set by scripts/install.sh via -ldflags "-X ...version.Version=vX.Y.Z".
var Version = "0.7.0"
