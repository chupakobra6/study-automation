package yandexmail

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

type Client interface {
	Search(folder string, since time.Time) ([]uint32, error)
	UIDValidity() uint32
	Fetch(uid uint32) ([]byte, error)
	Close() error
}

type IMAPClient struct {
	client      *imapclient.Client
	stopCancel  func() bool
	uidValidity uint32
}

func Connect(ctx context.Context, host string, port int, account, password string) (*IMAPClient, error) {
	address := net.JoinHostPort(host, strconv.Itoa(port))
	dialer := &tls.Dialer{
		Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, NextProtos: []string{"imap"}},
		NetDialer: &net.Dialer{Timeout: 30 * time.Second},
	}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("connect to Yandex IMAP: %w", err)
	}
	client := newIMAPClient(ctx, conn)
	if err := client.client.Login(account, password).Wait(); err != nil {
		client.Close()
		return nil, fmt.Errorf("login to Yandex IMAP: %w", err)
	}
	return client, nil
}

func newIMAPClient(ctx context.Context, conn net.Conn) *IMAPClient {
	// go-imap manages and resets its own per-response deadlines. Closing the
	// transport is what makes the source's absolute deadline interrupt all I/O.
	return &IMAPClient{
		client:     imapclient.New(conn, nil),
		stopCancel: context.AfterFunc(ctx, func() { _ = conn.Close() }),
	}
}

func (c *IMAPClient) Search(folder string, since time.Time) ([]uint32, error) {
	selected, err := c.client.Select(folder, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return nil, fmt.Errorf("select %q read-only: %w", folder, err)
	}
	c.uidValidity = selected.UIDValidity
	data, err := c.client.UIDSearch(&imap.SearchCriteria{Since: since}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("search %q: %w", folder, err)
	}
	uids := data.AllUIDs()
	result := make([]uint32, 0, len(uids))
	for _, uid := range uids {
		result = append(result, uint32(uid))
	}
	return result, nil
}

func (c *IMAPClient) UIDValidity() uint32 { return c.uidValidity }

func (c *IMAPClient) Fetch(uid uint32) ([]byte, error) {
	section := &imap.FetchItemBodySection{Peek: true}
	messages, err := c.client.Fetch(imap.UIDSetNum(imap.UID(uid)), &imap.FetchOptions{
		UID: true, InternalDate: true, BodySection: []*imap.FetchItemBodySection{section},
	}).Collect()
	if err != nil {
		return nil, fmt.Errorf("fetch UID %d: %w", uid, err)
	}
	if len(messages) != 1 {
		return nil, fmt.Errorf("fetch UID %d returned %d messages", uid, len(messages))
	}
	body := messages[0].FindBodySection(section)
	if len(body) == 0 {
		return nil, fmt.Errorf("fetch UID %d returned an empty body", uid)
	}
	return body, nil
}

func (c *IMAPClient) Close() error {
	c.stopCancel()
	// Collection is read-only; cleanup need not wait for a remote LOGOUT reply.
	return c.client.Close()
}
