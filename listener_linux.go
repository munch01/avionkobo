//go:build linux

package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type acceptCompatListener struct {
	file      *os.File
	fd        int
	address   net.Addr
	closeOnce sync.Once
	closeErr  error
}

type acceptCompatConn struct {
	fd        int
	address   net.Addr
	closeOnce sync.Once
	closeErr  error
}

const (
	armSocketcall    = 102
	acceptSocketcall = 5
)

func newDashboardListener(address string) (net.Listener, error) {
	tcpAddress, err := net.ResolveTCPAddr("tcp4", address)
	if err != nil {
		return nil, err
	}
	listener, err := net.ListenTCP("tcp4", tcpAddress)
	if err != nil {
		return nil, err
	}
	file, err := listener.File()
	if err != nil {
		listener.Close()
		return nil, err
	}
	address = listener.Addr().String()
	if err := listener.Close(); err != nil {
		file.Close()
		return nil, err
	}
	resolvedAddress, err := net.ResolveTCPAddr("tcp4", address)
	if err != nil {
		file.Close()
		return nil, err
	}
	return &acceptCompatListener{
		file:    file,
		fd:      int(file.Fd()),
		address: resolvedAddress,
	}, nil
}

func (listener *acceptCompatListener) Accept() (net.Conn, error) {
	for {
		var (
			fd    uintptr
			errno syscall.Errno
		)
		if runtime.GOARCH == "arm" {
			args := [3]uintptr{uintptr(listener.fd), 0, 0}
			fd, _, errno = syscall.Syscall(armSocketcall, acceptSocketcall, uintptr(unsafe.Pointer(&args[0])), 0)
			runtime.KeepAlive(args)
			if errors.Is(errno, syscall.ENOSYS) {
				fd, _, errno = syscall.Syscall(syscall.SYS_ACCEPT, uintptr(listener.fd), 0, 0)
				if errno != 0 {
					return nil, fmt.Errorf("accept unavailable via ARM socketcall (errno %d) and accept syscall (errno %d): %w", syscall.ENOSYS, errno, errno)
				}
			}
		} else {
			fd, _, errno = syscall.Syscall(syscall.SYS_ACCEPT, uintptr(listener.fd), 0, 0)
		}
		if errors.Is(errno, syscall.EINTR) {
			continue
		}
		if errno != 0 {
			return nil, fmt.Errorf("accept client connection (errno %d): %w", errno, errno)
		}
		syscall.CloseOnExec(int(fd))
		return &acceptCompatConn{fd: int(fd), address: listener.address}, nil
	}
}

func (connection *acceptCompatConn) Read(buffer []byte) (int, error) {
	for {
		count, err := syscall.Read(connection.fd, buffer)
		if !errors.Is(err, syscall.EINTR) {
			return count, err
		}
	}
}

func (connection *acceptCompatConn) Write(buffer []byte) (int, error) {
	written := 0
	for written < len(buffer) {
		count, err := syscall.Write(connection.fd, buffer[written:])
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		written += count
		if err != nil {
			return written, err
		}
		if count == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func (connection *acceptCompatConn) Close() error {
	connection.closeOnce.Do(func() {
		connection.closeErr = syscall.Close(connection.fd)
	})
	return connection.closeErr
}

func (connection *acceptCompatConn) LocalAddr() net.Addr {
	return connection.address
}

func (connection *acceptCompatConn) RemoteAddr() net.Addr {
	return connection.address
}

func (connection *acceptCompatConn) SetDeadline(deadline time.Time) error {
	if err := connection.SetReadDeadline(deadline); err != nil {
		return err
	}
	return connection.SetWriteDeadline(deadline)
}

func (connection *acceptCompatConn) SetReadDeadline(deadline time.Time) error {
	return connection.setSocketTimeout(syscall.SO_RCVTIMEO, deadline)
}

func (connection *acceptCompatConn) SetWriteDeadline(deadline time.Time) error {
	return connection.setSocketTimeout(syscall.SO_SNDTIMEO, deadline)
}

func (connection *acceptCompatConn) setSocketTimeout(option int, deadline time.Time) error {
	var timeout syscall.Timeval
	if !deadline.IsZero() {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			timeout.Usec = 1
		} else {
			timeout = syscall.NsecToTimeval(remaining.Nanoseconds())
			if timeout.Sec == 0 && timeout.Usec == 0 {
				timeout.Usec = 1
			}
		}
	}
	return syscall.SetsockoptTimeval(connection.fd, syscall.SOL_SOCKET, option, &timeout)
}

func (listener *acceptCompatListener) Close() error {
	listener.closeOnce.Do(func() {
		listener.closeErr = listener.file.Close()
	})
	return listener.closeErr
}

func (listener *acceptCompatListener) Addr() net.Addr {
	return listener.address
}
