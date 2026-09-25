package jobs

import (
	"bufio"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSMTP is a minimal SMTP server that counts connections and delivered
// messages, and refuses any recipient containing "reject".
type fakeSMTP struct {
	ln       net.Listener
	mu       sync.Mutex
	conns    int
	messages int
}

func newFakeSMTP(t *testing.T) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns++
			f.mu.Unlock()
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { c.Write([]byte(s + "\r\n")) }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-fake")
			say("250 8BITMIME")
		case strings.HasPrefix(cmd, "RCPT") && strings.Contains(cmd, "REJECT"):
			say("550 no such user")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"), cmd == "RSET", cmd == "NOOP":
			say("250 OK")
		case cmd == "DATA":
			say("354 go ahead")
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
			}
			f.mu.Lock()
			f.messages++
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("502 not implemented")
		}
	}
}

func (f *fakeSMTP) counts() (conns, messages int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conns, f.messages
}

// Audit O5: one SMTP connection per mail capped the queue at ~600 mails an
// hour. Now one run sends up to 100 mails over one connection; a refused
// recipient fails only its own mail, the rest still go out.
func TestMailJob_OneConnectionPerRun(t *testing.T) {
	f := newPurgeFixture(t)
	smtp := newFakeSMTP(t)
	f.s.cfg.SMTP.Host = "127.0.0.1"
	f.s.cfg.SMTP.Port, _ = strconv.Atoi(strings.Split(smtp.ln.Addr().String(), ":")[1])
	f.s.cfg.SMTP.TLS = "none"
	if err := f.stores.Settings.Save("mail.from_address", "ferri@example.com"); err != nil {
		t.Fatal(err)
	}
	enqueue := func(to string) {
		t.Helper()
		if err := f.stores.Mail.Enqueue(nil, to, "Hello", "<p>hi</p>", "hi"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 45; i++ {
		enqueue("r" + strconv.Itoa(i) + "@example.com")
	}

	f.s.runMailJob()
	conns, msgs := smtp.counts()
	if msgs != 45 || conns != 1 {
		t.Fatalf("45 queued mails: %d delivered over %d connections, want 45 over 1", msgs, conns)
	}

	enqueue("a@example.com")
	enqueue("reject@example.com")
	enqueue("b@example.com")
	f.s.runMailJob()
	if _, msgs := smtp.counts(); msgs != 47 {
		t.Fatalf("after a refused recipient: %d delivered in total, want 47 (the two good ones)", msgs)
	}
	var failed int
	f.d.QueryRow(`SELECT COUNT(*) FROM mail_queue WHERE to_address = 'reject@example.com' AND attempts = 1 AND status = 'pending'`).Scan(&failed)
	if failed != 1 {
		t.Fatal("the refused mail is not queued for a retry")
	}
}
