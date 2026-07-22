package redact

import (
	"regexp"
	"strings"
	"testing"
)

func TestBuiltins(t *testing.T) {
	cases := []struct {
		name, in   string
		wantGone   string // substring that must NOT survive
		wantKeep   string // substring that must survive (context preserved)
		wantMinHit int
	}{
		{"aws-key-id", "aws_access_key_id = AKIAIOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE", "aws_access_key_id", 1},
		{"github-pat", "remote: https://x@github.com token ghp_16C7e42F292c6912E7710c838347Ae178B4a", "ghp_16C7e42F292c6912E7710c838347Ae178B4a", "remote:", 1},
		{"github-fine", "using github_pat_11ABCDEFG0123456789_abcdefghij", "github_pat_11ABCDEFG0123456789_abcdefghij", "using", 1},
		{"slack", "SLACK_TOKEN=xoxb-1234567890-abcdefghijklm", "xoxb-1234567890-abcdefghijklm", "SLACK_TOKEN", 1},
		{"stripe", "key: sk_live" + "_4eC39HqLyjWDarjtT1zdp7dc", "sk_live" + "_4eC39HqLyjWDarjtT1zdp7dc", "key:", 1},
		{"openai", "OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz123456", "sk-proj-abcdefghijklmnop", "OPENAI_API_KEY", 1},
		{"google", "curl 'https://maps.google/?key=AIzaSyA-1234567890abcdefghijklmnopqrstu'", "AIzaSyA-1234567890abcdefghijklmnopqrstu", "maps.google", 1},
		{"jwt", "token eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0In0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJVadQssw5c", "SflKxwRJSMeKKF2QT4fwpMeJf36", "token ", 1},
		{"url-creds", "postgres://admin:hunter2@db.internal:5432/app", "hunter2", "postgres://admin:", 1},
		{"auth-header", `curl -H "Authorization: Bearer abc123def456" api.example.com`, "abc123def456", "api.example.com", 1},
		{"generic-password", "password=SuperSecret99", "SuperSecret99", "password=", 1},
		{"generic-colon", "api_key: 0123456789abcdef", "0123456789abcdef", "api_key:", 1},
		{"npm", "//registry.npmjs.org/:_authToken=npm_abcdEFGHijklMNOPqrstUVWXyz0123456789", "npm_abcdEFGHijklMNOPqrstUVWXyz0123456789", "registry.npmjs.org", 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, n := String(c.in, nil)
			if strings.Contains(out, c.wantGone) {
				t.Errorf("secret survived: %q in %q", c.wantGone, out)
			}
			if !strings.Contains(out, c.wantKeep) {
				t.Errorf("context lost: want %q in %q", c.wantKeep, out)
			}
			if !strings.Contains(out, "[REDACTED]") {
				t.Errorf("no mask in %q", out)
			}
			if n < c.wantMinHit {
				t.Errorf("hits = %d, want >= %d", n, c.wantMinHit)
			}
		})
	}
}

func TestPrivateKeyBlock(t *testing.T) {
	in := "junk before\n-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXk\nmore==\n-----END OPENSSH PRIVATE KEY-----\njunk after"
	out, n := String(in, nil)
	if n != 1 {
		t.Fatalf("n = %d, want 1", n)
	}
	if strings.Contains(out, "b3BlbnNzaC1rZXk") {
		t.Errorf("key material survived: %q", out)
	}
	for _, keep := range []string{"junk before", "junk after", "-----BEGIN OPENSSH PRIVATE KEY-----", "-----END OPENSSH PRIVATE KEY-----"} {
		if !strings.Contains(out, keep) {
			t.Errorf("want %q kept in %q", keep, out)
		}
	}
}

func TestCleanTextUntouched(t *testing.T) {
	for _, in := range []string{
		"ls -la /tmp",
		"error: connection refused on port 5432",
		"the token bucket rate limiter", // bare word "token" with no value
		"drwxr-xr-x 2 root root 4096 Jul 22 10:00 bin",
		"100 files changed, 2 insertions(+)",
	} {
		out, n := String(in, nil)
		if n != 0 || out != in {
			t.Errorf("clean text changed: %q -> %q (n=%d)", in, out, n)
		}
	}
}

func TestExtraPatterns(t *testing.T) {
	extra := []*regexp.Regexp{regexp.MustCompile(`ACME-[0-9]{6}`)}
	out, n := String("order id ACME-123456 shipped", extra)
	if n != 1 || strings.Contains(out, "ACME-123456") {
		t.Errorf("extra pattern not applied: %q (n=%d)", out, n)
	}
}

func TestIdempotent(t *testing.T) {
	in := "password=hunter22 and ghp_16C7e42F292c6912E7710c838347Ae178B4a"
	once, _ := String(in, nil)
	twice, n := String(once, nil)
	if twice != once {
		t.Errorf("not idempotent: %q vs %q", once, twice)
	}
	_ = n
}
