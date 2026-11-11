//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"unsafe"
)

// AF_VSOCK constants of the Linux kernel. They are declared here instead of
// pulling in an external package because the guest agent is the only place in
// the project that speaks vsock, and a dependency for two constants is not
// worth it.
const (
	afVsock         = 40
	soVmSocketConst = 1
	// VMADDR_CID_ANY makes the listener accept connections addressed to this
	// guest. CID 2 is VMADDR_CID_HOST and is never valid as a guest bind address.
	vmaddrCIDAny = ^uint32(0)
)

// sockaddrVM mirrors struct sockaddr_vm from <linux/vm_sockets.h>.
type sockaddrVM struct {
	family uint16
	_      [2]byte
	port   uint32
	cid    uint32
	_      [4]byte
}

// listenVsock opens an AF_VSOCK listener on the guest's own cid. Connecting to
// the guest from the host is what lets the adapter run commands inside the
// microVM without a network stack the task could use to escape.
func listenVsock(port uint32) (net.Listener, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_SOCKET, afVsock, syscall.SOCK_STREAM, 0)
	if errno != 0 {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", errno)
	}
	addr := &sockaddrVM{family: afVsock, port: port, cid: vmaddrCIDAny}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, fd,
		uintptr(unsafe.Pointer(addr)), unsafe.Sizeof(*addr)); errno != 0 {
		syscall.Close(int(fd))
		return nil, fmt.Errorf("bind vsock %d: %w", port, errno)
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_LISTEN, fd, 128, 0); errno != 0 {
		syscall.Close(int(fd))
		return nil, fmt.Errorf("listen vsock %d: %w", port, errno)
	}
	// syscall.Accept is not wrapped by net.Fd on a raw fd, so the listener is
	// built from the file descriptor directly.
	file := os.NewFile(fd, fmt.Sprintf("vsock:%d", port))
	if file == nil {
		syscall.Close(int(fd))
		return nil, fmt.Errorf("vsock listener fd %d is not a file", fd)
	}
	return net.FileListener(file)
}

var _ = soVmSocketConst
