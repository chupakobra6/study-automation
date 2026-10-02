package yandexmail

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestSourceDeadlineInterruptsStalledIMAPCommand(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer serverConn.Close()
	commandReceived := make(chan struct{})
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		if _, err := io.WriteString(serverConn, "* OK [CAPABILITY IMAP4rev1] test ready\r\n"); err != nil {
			return
		}
		if _, err := bufio.NewReader(serverConn).ReadString('\n'); err != nil {
			return
		}
		close(commandReceived)
		// The server keeps the connection open but never answers LOGIN.
		_, _ = io.Copy(io.Discard, serverConn)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	client := newIMAPClient(ctx, clientConn)
	defer client.Close()
	started := time.Now()
	if err := client.client.Login("fixture-account", "fixture-password").Wait(); err == nil {
		t.Fatal("stalled command unexpectedly succeeded")
	}
	if ctx.Err() != context.DeadlineExceeded || time.Since(started) > 2*time.Second {
		t.Fatalf("deadline did not interrupt command: %v after %v", ctx.Err(), time.Since(started))
	}
	select {
	case <-commandReceived:
	default:
		t.Fatal("server did not receive the command")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("transport remained open after deadline")
	}
}
