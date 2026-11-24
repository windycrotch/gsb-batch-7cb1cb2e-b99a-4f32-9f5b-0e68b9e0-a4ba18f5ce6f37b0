// Package redact removes userinfo (username and password) from RTSP and RTSPS
// URLs embedded in free-form error text, while preserving the scheme, host,
// port and path so failures remain diagnosable from logs.
package redact

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// rtspURLPattern matches rtsp:// and rtsps:// URLs up to the first whitespace.
// The match is case-insensitive and deliberately permissive because camera
// URLs and ffmpeg log lines can contain percent-escapes or technically
// malformed authority sections that a strict URL parser would reject.
var rtspURLPattern = regexp.MustCompile(`(?i)rtsps?://[^\s]+`)

const redactedUserinfo = "REDACTED"

// URL returns an RTSP/RTSPS URL with any username and password replaced by
// REDACTED, keeping scheme, host, port, path and query intact. URLs without
// credentials, as well as strings that cannot be parsed as a URL, are
// returned unchanged.
func URL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u == nil || u.User == nil {
		return rawURL
	}
	u.User = url.User(redactedUserinfo)
	return u.String()
}

// Text rewrites every rtsp:// or rtsps:// URL found in text, hiding embedded
// credentials while leaving the remainder of the line untouched. Credential-
// free URLs and malformed URLs without an authority "@" are left as-is.
func Text(text string) string {
	return rtspURLPattern.ReplaceAllStringFunc(text, redactMatch)
}

// Error returns an error whose message is err's message with every embedded
// RTSP/RTSPS URL redacted. The returned error intentionally does not wrap
// err: wrapping would keep the original message reachable through
// errors.Unwrap and leave the secret recoverable. A nil error passes through
// unchanged.
func Error(err error) error {
	if err == nil {
		return nil
	}
	return &redactedError{msg: Text(err.Error())}
}

// Errorf formats an error message and redacts any RTSP/RTSPS URLs in it.
func Errorf(format string, args ...any) error {
	return Error(fmt.Errorf(format, args...))
}

type redactedError struct {
	msg string
}

func (e *redactedError) Error() string { return e.msg }

func redactMatch(rawURL string) string {
	schemeEnd := strings.Index(rawURL, "://")
	if schemeEnd < 0 {
		return rawURL
	}
	authorityStart := schemeEnd + len("://")
	tail := rawURL[authorityStart:]

	authorityEnd := len(tail)
	if i := strings.IndexAny(tail, "/?#"); i >= 0 {
		authorityEnd = i
	}

	at := strings.LastIndex(tail[:authorityEnd], "@")
	if at < 0 {
		return rawURL
	}
	return rawURL[:authorityStart] + redactedUserinfo + "@" + tail[at+1:]
}
