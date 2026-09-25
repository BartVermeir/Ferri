package storage

import (
	"errors"
	"os"
	"testing"

	smb2 "github.com/hirochachacha/go-smb2"
)

// An SMB server that ends an idle session answers with an NT status whose
// text has none of the connection keywords; it must still count as "reconnect
// and retry", also when go-smb2 wraps it in an *os.PathError (audit M6).
func TestIsConnectionError_ServerEndedSession(t *testing.T) {
	for _, code := range []uint32{0xC000035C, 0xC0000203, 0xC00000C9} {
		err := &os.PathError{Op: "open", Path: "x.info", Err: &smb2.ResponseError{Code: code}}
		if !isConnectionError(err) {
			t.Errorf("NT status %#x (%v) not treated as a lost session", code, err)
		}
	}
	notFound := &os.PathError{Op: "open", Path: "x.info", Err: &smb2.ResponseError{Code: 0xC0000034}} // OBJECT_NAME_NOT_FOUND
	if isConnectionError(notFound) {
		t.Error("a missing file is treated as a lost connection")
	}
	if !isConnectionError(errors.New("write tcp: connection reset by peer")) {
		t.Error("a reset connection is no longer treated as one")
	}
}

// With no share (an earlier reconnect failed), withShare must try to connect
// again instead of calling fn with a nil share. The host is unusable, so the
// attempt fails fast and fn is never called.
func TestWithShare_NoShareReconnectsInsteadOfNil(t *testing.T) {
	b := &SMBBackend{cfg: SMBConfig{Host: "127.0.0.1:1"}}
	called := false
	err := b.withShare(func(*smb2.Share) error { called = true; return nil })
	if called {
		t.Fatal("fn was called with a nil share")
	}
	if err == nil {
		t.Fatal("no error without a connection")
	}
}
