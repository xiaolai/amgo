// Package version holds the build version injected by the linker.
package version

// Version is the user-visible release version. Build scripts override it with
// -X amgo/internal/version.Version=<version>.
var Version = "0.2.0"
