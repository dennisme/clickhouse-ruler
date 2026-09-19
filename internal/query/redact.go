package query

import "strings"

// redact removes a password from text before it is returned or logged.
//
// Connection options are built from fields rather than a DSN, so a driver
// error has no reason to contain the credential. This is a backstop for the
// cases where it does anyway, since an error message is the easiest way for a
// secret to reach a log.
func redact(text, password string) string {
	if password == "" {
		return text
	}
	return strings.ReplaceAll(text, password, "xxxxx")
}
