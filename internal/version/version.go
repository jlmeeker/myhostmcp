// Package version holds the build version of the myhostmcp binary.
package version

// Version is reported by both the local and remote halves so that the local
// half can detect a stale remote binary and re-upload.
const Version = "0.2.0-dev"
