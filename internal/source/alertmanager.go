package source

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// DefaultAuthorizationType is what Prometheus defaults `authorization.type`
// to, and an operator who wrote only a credential means the bearer case.
const DefaultAuthorizationType = "Bearer"

// Alertmanager is one set of Alertmanager endpoints and the credential used to
// reach them.
//
// Every URL in a set is posted to, because Alertmanager's clustering expects a
// sender to reach every member rather than a balancer in front of them, and
// the set is what they share: one cluster, one credential (spec 6.5).
type Alertmanager struct {
	URLs []string

	// At most one of these is set. Both write the same Authorization header,
	// so a set carrying both is refused rather than resolved.
	BasicAuth     *BasicAuth
	Authorization *Authorization

	// TLS is the transport the HTTP client is built with, nil for a set every
	// member of which is reached over plaintext or over the host's own trust
	// store. The same material a source carries, read by the same code, and
	// held the same way: the CA as bytes, the client pair as the two paths
	// (spec 6.5, 6.2).
	TLS *TLS

	// Exemptions are what clears alertmanager/tls-insecure, with a reason and
	// a date, which is the mechanism a source already carries (spec 7.7).
	Exemptions []Exemption

	lines lint.Lines
}

// Exempts reports whether this set has an unexpired exemption for a check.
func (a Alertmanager) Exempts(check string, now time.Time) bool {
	return exempts(a.Exemptions, check, now)
}

// Subject is what a finding about this set is reported against.
//
// The first member rather than a name, because a set has none: it is one
// cluster, and the address an operator wrote first is what identifies it in a
// finding, in a log line and on the alertmanager metric label.
func (a Alertmanager) Subject() string {
	if len(a.URLs) == 0 {
		return ""
	}
	return a.URLs[0]
}

// BasicAuth is Prometheus' own `basic_auth`, with the secret already resolved
// out of the file or the environment it was named in.
type BasicAuth struct {
	Username string
	Password string
}

// Authorization is Prometheus' own `authorization`, which is the bearer token
// case, with the credential already resolved.
type Authorization struct {
	Type        string
	Credentials string
}

// Credential reports the Authorization header value this set authenticates
// with, and whether it has one at all.
//
// Built here rather than by the caller so that a credential has one spelling,
// and so that a set carrying none is a set that adds no header rather than one
// that adds an empty one.
func (a Alertmanager) Credential() (string, bool) {
	switch {
	case a.BasicAuth != nil:
		raw := a.BasicAuth.Username + ":" + a.BasicAuth.Password
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(raw)), true
	case a.Authorization != nil:
		return a.Authorization.Type + " " + a.Authorization.Credentials, true
	}
	return "", false
}

func parseAlertmanagers(r *lint.Reader, n *yaml.Node, env func(string) (string, bool)) []Alertmanager {
	if !r.Sequence(n, "alertmanagers") {
		return nil
	}

	sets := make([]Alertmanager, 0, len(n.Content))
	for i, item := range n.Content {
		if !r.Mapping(item, "alertmanager") {
			continue
		}
		// One set is what ships. A list is the shape so that a second set is
		// additive later rather than a rewrite, but selecting between sets
		// would be routing and Alertmanager owns that, so there is nothing yet
		// for a second one to mean (spec 6.5).
		if i > 0 {
			r.Add(item.Line, lint.CheckAlertmanagerURL, lint.SeverityError,
				"only one alertmanagers entry is supported: list every member of the cluster in its urls")
			continue
		}
		sets = append(sets, parseAlertmanager(r, item, env))
	}
	return sets
}

func parseAlertmanager(r *lint.Reader, n *yaml.Node, env func(string) (string, bool)) Alertmanager {
	a := Alertmanager{lines: lint.NewLines(n.Line)}
	var basic *basicAuthRef
	var authz *authorizationRef
	var tlsConfig tlsRef

	// How many urls the operator wrote, as opposed to how many survived
	// validation. A set whose one url is a typo has already been told so, and
	// saying "urls is empty" beside that reports one mistake twice.
	var listed int

	for _, e := range lint.Entries(n) {
		a.lines.Set(e.Key.Value, e.Key.Line)
		switch e.Key.Value {
		case "urls":
			a.URLs, listed = parseAlertmanagerURLs(r, e.Value)
		case "basic_auth":
			basic = parseBasicAuth(r, e.Value)
		case "authorization":
			authz = parseAuthorization(r, e.Value)
		case "tls_config":
			tlsConfig = parseTLSConfig(r, e.Value, a.lines)
		case "exempt":
			a.Exemptions = parseExemptions(r, e.Value)
		default:
			r.UnknownField(e.Key, "alertmanager")
		}
	}

	a.TLS = a.resolveTLS(r, tlsConfig)

	if listed == 0 {
		r.Add(a.lines.Of("urls"), lint.CheckAlertmanagerURL, lint.SeverityError,
			"urls is empty, expected at least one Alertmanager to post to")
	}

	// Both write the same Authorization header, so one of them would silently
	// win. Refused for the reason password_file and password_env are.
	if basic != nil && authz != nil {
		r.Add(a.lines.Of("basic_auth", "authorization"), lint.CheckAlertmanagerAuth, lint.SeverityError,
			"basic_auth and authorization are mutually exclusive, set one")
		return a
	}

	switch {
	case basic != nil:
		if basic.username == "" {
			r.Add(a.lines.Of("basic_auth"), lint.CheckAlertmanagerAuth, lint.SeverityError,
				"basic_auth has no username")
			return a
		}
		password := resolveSecret(r, basic.line, lint.CheckAlertmanagerAuth, "password", basic.secret, env)
		a.BasicAuth = &BasicAuth{Username: basic.username, Password: password}

	case authz != nil:
		credentials := resolveSecret(r, authz.line, lint.CheckAlertmanagerAuth, "credentials", authz.secret, env)
		kind := authz.kind
		if kind == "" {
			kind = DefaultAuthorizationType
		}
		a.Authorization = &Authorization{Type: kind, Credentials: credentials}
	}

	return a
}

// resolveTLS reads the material a set's tls_config names, nil for a set that
// configured none.
//
// The scheme is what turns TLS on, because an endpoint is a URL and `https://`
// already says what a source's `secure: true` says. So material that no member
// of the set can reach is refused rather than ignored, which is the answer
// `secure: false` beside a tls_config gets for the same reason: the ruler would
// post in plaintext while a reviewer reads a file that names a CA (spec 6.5).
//
// Every member rather than one of them. The set is one cluster sharing one
// client, so members that disagree about their transport are a mistake in the
// file rather than a topology to support.
func (a Alertmanager) resolveTLS(r *lint.Reader, ref tlsRef) *TLS {
	if !ref.set {
		return nil
	}

	for _, u := range a.URLs {
		if !strings.HasPrefix(u, "https://") {
			r.Add(a.lines.Of("tls_config"), lint.CheckAlertmanagerTLS, lint.SeverityError,
				"tls_config needs every url to be https://, and %q is not", u)
			return nil
		}
	}

	return tlsReader{r: r, lines: a.lines, check: lint.CheckAlertmanagerTLS}.resolve(ref)
}

// parseAlertmanagerURLs reads and validates the urls of one set.
//
// Every value is validated, because the second address being a typo is no less
// silent than the first one being one.
// It returns the urls that validated and how many were written, so the caller
// can tell an absent list from one whose every entry was refused.
func parseAlertmanagerURLs(r *lint.Reader, n *yaml.Node) (urls []string, listed int) {
	if !r.Sequence(n, "urls") {
		return nil, 0
	}

	urls = make([]string, 0, len(n.Content))
	listed = len(n.Content)
	seen := make(map[string]struct{}, len(n.Content))

	for _, item := range n.Content {
		raw, ok := r.Scalar(item, "url")
		if !ok {
			continue
		}

		u, err := parseAlertmanagerURL(raw)
		if err != nil {
			r.Add(item.Line, lint.CheckAlertmanagerURL, lint.SeverityError, "%s", err.Error())
			continue
		}

		// A trailing slash is the same endpoint: notify.Client trims one
		// before it builds a request path.
		//
		// The same address twice is refused. It is one page posted twice to
		// one member, which Alertmanager deduplicates, so nothing breaks and
		// nothing is gained; what it does break is the metrics, because the
		// `alertmanager` label is the URL and two identical values are one
		// series, so a failure counter would report two endpoints as one
		// (spec 6.5).
		key := strings.TrimSuffix(u.String(), "/")
		if _, dup := seen[key]; dup {
			r.Add(item.Line, lint.CheckAlertmanagerURL, lint.SeverityError,
				"%q: given twice, list each member of the cluster once", key)
			continue
		}
		seen[key] = struct{}{}

		urls = append(urls, key)
	}
	return urls, listed
}

// parseAlertmanagerURL validates one Alertmanager URL.
//
// Four things make it unusable, and none of them is reachability, because that
// needs the network and changes while the ruler runs: no scheme, a scheme
// net/http will not speak, no host to send to, or a credential in the URL.
//
// The error never echoes the value. url.Parse prints what it was given,
// userinfo and all, and a finding carrying a password puts it in the CI log
// this check exists to keep it out of (spec 8.4).
func parseAlertmanagerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			err = uerr.Err
		}
		return nil, fmt.Errorf("not a URL: %w, want one like http://localhost:9093", err)
	}

	// Before the scheme and host, because those report the URL back and this
	// is the one case where it may not be repeated. Go's http.Client turns
	// userinfo into an Authorization header, so this would authenticate, which
	// is why it is refused rather than ignored: a password in a URL is in the
	// pod spec, the rendered chart manifest and any dump that echoes argv.
	if u.User != nil {
		return nil, fmt.Errorf("carries a credential in the URL, use basic_auth or authorization instead")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%q: want a http:// or https:// URL, e.g. http://localhost:9093", raw)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("%q: no host to send alerts to, want one like http://localhost:9093", raw)
	}
	return u, nil
}

// basicAuthRef and authorizationRef are a credential block as written, read
// before anything is resolved so that the refusals can name the line the
// operator wrote rather than the line the set starts on.
type basicAuthRef struct {
	username string
	secret   secretRef
	line     int
}

type authorizationRef struct {
	kind   string
	secret secretRef
	line   int
}

func parseBasicAuth(r *lint.Reader, n *yaml.Node) *basicAuthRef {
	if !r.Mapping(n, "basic_auth") {
		return nil
	}
	ref := &basicAuthRef{line: n.Line}

	for _, e := range lint.Entries(n) {
		switch e.Key.Value {
		case "username":
			ref.username, _ = r.Scalar(e.Value, "username")
		case "password_file":
			ref.secret.file, _ = r.Scalar(e.Value, "password_file")
			ref.line = e.Key.Line
		case "password_env":
			ref.secret.env, _ = r.Scalar(e.Value, "password_env")
			ref.line = e.Key.Line
		default:
			r.UnknownField(e.Key, "basic_auth")
		}
	}
	return ref
}

func parseAuthorization(r *lint.Reader, n *yaml.Node) *authorizationRef {
	if !r.Mapping(n, "authorization") {
		return nil
	}
	ref := &authorizationRef{line: n.Line}

	for _, e := range lint.Entries(n) {
		switch e.Key.Value {
		case "type":
			ref.kind, _ = r.Scalar(e.Value, "type")
		case "credentials_file":
			ref.secret.file, _ = r.Scalar(e.Value, "credentials_file")
			ref.line = e.Key.Line
		case "credentials_env":
			ref.secret.env, _ = r.Scalar(e.Value, "credentials_env")
			ref.line = e.Key.Line
		default:
			r.UnknownField(e.Key, "authorization")
		}
	}
	return ref
}
