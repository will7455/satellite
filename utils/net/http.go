package net

import (
	"io"
	"net/url"
	"strings"
)

// IsProbableEOF returns true if the given error resembles a connection termination
// scenario that would justify assuming that the watch is empty.
// These errors are what the Go http stack returns back to us which are general
// connection closure errors (strongly correlated) and callers that need to
// differentiate probable errors in connection behavior between normal "this is
// disconnected" should use the method.
func IsProbableEOF(err error) bool {
	if err == nil {
		return false
	}
	if uerr, ok := err.(*url.Error); ok {
		err = uerr.Err
	}
	msg := err.Error()
	switch {
	case err == io.EOF:
		return true
	case msg == "http: can't write HTTP request on broken connection":
		return true
	case strings.Contains(msg, "http2: server sent GOAWAY and closed the connection"):
		return true
	case strings.Contains(msg, "connection reset by peer"):
		return true
	case strings.Contains(strings.ToLower(msg), "use of closed network connection"):
		return true
	}
	return false
}
