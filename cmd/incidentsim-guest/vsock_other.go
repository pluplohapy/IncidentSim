//go:build !linux

package main

import (
	"errors"
	"net"
)

// listenVsock is only meaningful inside a Linux guest. Building the agent on a
// developer machine (macOS for instance) must still succeed, so the unsupported
// platform fails at runtime instead of breaking the build.
func listenVsock(port uint32) (net.Listener, error) {
	return nil, errors.New("AF_VSOCK is only available inside a Linux guest: build the agent with GOOS=linux")
}
