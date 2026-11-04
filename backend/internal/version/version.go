// Package version exposes the build version, set at link time:
//
//	go build -ldflags "-X campaign-assistant/backend/internal/version.Version=v1.2.3"
package version

// Version is the build version. It stays "dev" for unstamped builds.
var Version = "dev"
