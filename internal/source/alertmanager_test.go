package source

import (
	"strings"
	"testing"

	"github.com/dennisme/clickhouse-ruler/internal/lint"
)

// parseAM parses a document holding only an alertmanagers block, so a test
// reads as the block it is about rather than as a whole file.
func parseAM(t *testing.T, body string) (*File, []lint.Problem) {
	t.Helper()
	return Parse("ruler.yaml", []byte(body), fixtureEnv)
}

// The order the operator wrote is the order kept. It reaches nothing
// functional, since every member is posted to, but a list that reordered itself
// reads as a different configuration than the one in the file.
func TestAlertmanagerURLsAreRead(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls:
      - http://am-0:9093
      - http://am-1:9093
`)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	if len(f.Alertmanagers) != 1 {
		t.Fatalf("alertmanagers = %d, want 1", len(f.Alertmanagers))
	}
	got := f.Alertmanagers[0].URLs
	if len(got) != 2 || got[0] != "http://am-0:9093" || got[1] != "http://am-1:9093" {
		t.Errorf("urls = %v, want both members in order", got)
	}
}

func TestAlertmanagerBasicAuthResolvesFromAFile(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_file: testdata/secrets/traces
`)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	auth := f.Alertmanagers[0].BasicAuth
	if auth == nil {
		t.Fatal("basic_auth was not read")
	}
	if auth.Username != "ruler" {
		t.Errorf("username = %q, want ruler", auth.Username)
	}
	if auth.Password == "" {
		t.Error("password was not resolved from password_file")
	}
}

func TestAlertmanagerBasicAuthResolvesFromTheEnvironment(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_env: RULER_PASSWORD_LOGS
`)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	if got := f.Alertmanagers[0].BasicAuth.Password; got != "log-env-secret" {
		t.Errorf("password = %q, want the value from the environment", got)
	}
}

func TestAlertmanagerBearerTokenResolves(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls: [http://am:9093]
    authorization:
      credentials_file: testdata/secrets/traces
`)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	auth := f.Alertmanagers[0].Authorization
	if auth == nil {
		t.Fatal("authorization was not read")
	}
	// Prometheus defaults the type to Bearer, and an operator who wrote only a
	// credentials_file means the bearer case.
	if auth.Type != "Bearer" {
		t.Errorf("type = %q, want Bearer by default", auth.Type)
	}
	if auth.Credentials == "" {
		t.Error("credentials were not resolved from credentials_file")
	}
}

// No credential at all is legal: the compose stack runs an Alertmanager on a
// network only the ruler can reach.
func TestAlertmanagerNeedsNoCredential(t *testing.T) {
	f, problems := parseAM(t, `
alertmanagers:
  - urls: [http://am:9093]
`)
	if len(problems) != 0 {
		t.Fatalf("expected no problems, got %v", problems)
	}
	if f.Alertmanagers[0].BasicAuth != nil || f.Alertmanagers[0].Authorization != nil {
		t.Error("a set with no credential read one anyway")
	}
}

func TestAlertmanagerRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  string
		check string
		want  string
	}{
		{
			name: "both password sources",
			body: `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_file: testdata/secrets/traces
      password_env: RULER_PASSWORD_LOGS
`,
			check: lint.CheckAlertmanagerAuth,
			want:  "password_file and password_env are mutually exclusive, set one",
		},
		{
			name: "unreadable password file",
			body: `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_file: testdata/secrets/nowhere
`,
			check: lint.CheckAlertmanagerAuth,
			want:  `cannot read password_file "testdata/secrets/nowhere"`,
		},
		{
			name: "empty password file",
			body: `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_file: testdata/secrets/empty
`,
			check: lint.CheckAlertmanagerAuth,
			want:  `password_file "testdata/secrets/empty" is empty`,
		},
		{
			name: "missing environment variable",
			body: `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_env: RULER_NOWHERE
`,
			check: lint.CheckAlertmanagerAuth,
			want:  "password_env references ${RULER_NOWHERE}, which is not set in the environment",
		},
		{
			name: "basic auth with no username",
			body: `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      password_file: testdata/secrets/traces
`,
			check: lint.CheckAlertmanagerAuth,
			want:  "basic_auth has no username",
		},
		{
			name: "both auth blocks",
			body: `
alertmanagers:
  - urls: [http://am:9093]
    basic_auth:
      username: ruler
      password_file: testdata/secrets/traces
    authorization:
      credentials_file: testdata/secrets/traces
`,
			check: lint.CheckAlertmanagerAuth,
			want:  "basic_auth and authorization are mutually exclusive",
		},
		{
			name: "no urls",
			body: `
alertmanagers:
  - basic_auth:
      username: ruler
      password_file: testdata/secrets/traces
`,
			check: lint.CheckAlertmanagerURL,
			want:  "urls is empty, expected at least one Alertmanager to post to",
		},
		{
			name: "url with no scheme",
			body: `
alertmanagers:
  - urls: [am:9093]
`,
			check: lint.CheckAlertmanagerURL,
			want:  "want a http:// or https:// URL",
		},
		{
			name: "url with no host",
			body: `
alertmanagers:
  - urls: [http://]
`,
			check: lint.CheckAlertmanagerURL,
			want:  "no host to send alerts to",
		},
		{
			name: "url carrying a credential",
			body: `
alertmanagers:
  - urls: [http://ruler:hunter2@am:9093]
`,
			check: lint.CheckAlertmanagerURL,
			want:  "carries a credential in the URL, use basic_auth",
		},
		{
			name: "the same url twice",
			body: `
alertmanagers:
  - urls: [http://am:9093, http://am:9093/]
`,
			check: lint.CheckAlertmanagerURL,
			want:  "given twice, list each member of the cluster once",
		},
		{
			name: "a second set",
			body: `
alertmanagers:
  - urls: [http://am-a:9093]
  - urls: [http://am-b:9093]
`,
			check: lint.CheckAlertmanagerURL,
			want:  "only one alertmanagers entry is supported",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, problems := parseAM(t, tc.body)
			if !reported(problems, tc.check, tc.want) {
				t.Errorf("expected %s saying %q, got %v", tc.check, tc.want, problems)
			}
		})
	}
}

// A URL that carries a credential is never echoed back, because the finding
// would then put the password in the CI log that the check exists to keep it
// out of.
func TestAlertmanagerFindingsNeverEchoACredential(t *testing.T) {
	_, problems := parseAM(t, `
alertmanagers:
  - urls: [http://ruler:hunter2@am:9093]
`)
	for _, p := range problems {
		if strings.Contains(p.Text, "hunter2") {
			t.Fatalf("a finding echoed the password: %s", p.Text)
		}
	}
}

func reported(problems []lint.Problem, check, want string) bool {
	for _, p := range problems {
		if p.Check == check && strings.Contains(p.Text, want) {
			return true
		}
	}
	return false
}
