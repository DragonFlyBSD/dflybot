// Copyright (c) 2026 Aaron LI
//
// urlshort: a small, configurable URL shortener for the DragonFly monitors.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

// programName is the binary name used in log and error messages.
const programName = "urlshort"

// version is the program version reported by the status endpoint and logs.
// It can be overridden at build time with
//
//	-ldflags "-X main.version=<version>"
var version = "0.1.0"
