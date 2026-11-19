package tools

import (
	"strings"
	"testing"
)

// TestRedactionRemovesEverySecretClass covers each documented class: one marker
// per match, and no substring of the secret survives.
func TestRedactionRemovesEverySecretClass(t *testing.T) {
	t.Parallel()
	// The fixtures are assembled at run time so that no secret-shaped literal
	// lives in the repository, while the assertions still prove the whole
	// secret is gone.
	privateKeyBody := "MIIEowIBAAKCAQEA" + strings.Repeat("a", 24)
	privateKey := "-----BEGIN " + "RSA PRIVATE KEY-----\n" + privateKeyBody +
		"\n-----END " + "RSA PRIVATE KEY-----"
	jwt := "eyJ" + "hbGciOiJIUzI1NiJ9" + "." + "eyJ" + "zdWIiOiIxMjM0NTY3ODkwIn0" +
		"." + strings.Repeat("a", 43)
	awsKey := "AKIA" + strings.Repeat("A", 16)
	githubToken := "ghp_" + strings.Repeat("a", 36)
	providerKey := "sk-" + strings.Repeat("b", 24)
	cases := []struct {
		name   string
		input  string
		class  string
		secret string
	}{
		{"bearer token", "Authorization: Bearer abcdef1234567890xyz", RedactedBearer, "abcdef1234567890xyz"},
		{"JWT", "token=" + jwt, RedactedJWT, jwt},
		{"PEM block", "key:\n" + privateKey, RedactedPEM, privateKeyBody},
		{"AWS key id", "aws_access_key_id = " + awsKey, RedactedAWSAccessKey, awsKey},
		{"GitHub token", "GITHUB_TOKEN=" + githubToken, RedactedGitHub, githubToken},
		{"provider key", "api key " + providerKey, RedactedProvider, providerKey},
		{"credential assignment", `password: "hunter2hunter2"`, RedactedCredential, "hunter2hunter2"},
		{"long base64", "data: " + strings.Repeat("QWERTYUIOPasdfghjkl", 6),
			RedactedBase64, strings.Repeat("QWERTYUIOPasdfghjkl", 6)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			redacted, count := Redact(tc.input)
			if count != 1 {
				t.Errorf("redactions = %d, want one for %s", count, tc.name)
			}
			if marker := "[" + "redacted:" + tc.class + "]"; !strings.Contains(redacted, marker) {
				t.Errorf("redacted text = %q, want the marker %s", redacted, marker)
			}
			if strings.Contains(redacted, tc.secret) {
				t.Errorf("redacted text still contains the secret %q", tc.secret)
			}
			if strings.Count(redacted, "[redacted:") != 1 {
				t.Errorf("redacted text = %q, want exactly one marker", redacted)
			}
		})
	}
}

// TestRedactionCountsEveryMatch checks that two secrets yield two markers.
func TestRedactionCountsEveryMatch(t *testing.T) {
	t.Parallel()
	first := "AKIA" + strings.Repeat("A", 16)
	second := "ASIA" + strings.Repeat("B", 16)
	redacted, count := Redact("aws " + first + " and " + second)
	if count != 2 {
		t.Errorf("redactions = %d, want two", count)
	}
	if strings.Count(redacted, "[redacted:") != 2 {
		t.Errorf("redacted = %q, want two markers", redacted)
	}
}

// TestRedactionLeavesOrdinaryTextAlone guards against a pattern that eats prose.
func TestRedactionLeavesOrdinaryTextAlone(t *testing.T) {
	t.Parallel()
	const prose = "The container was OOMKilled after allocating 512 MiB at 14:02:11Z."
	redacted, count := Redact(prose)
	if count != 0 || redacted != prose {
		t.Errorf("Redact(%q) = %q, %d, want it unchanged", prose, redacted, count)
	}
}
