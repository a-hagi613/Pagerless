package cli

import (
	"testing"
)

func TestParseTarget(t *testing.T) {
	cases := []struct {
		input   string
		want    *target
		wantErr bool
	}{
		{"", nil, false},
		{testCheckout, &target{Name: testCheckout}, false},
		{"Deployment/checkout", &target{Kind: testKind, Name: testCheckout}, false},
		{"shop/Deployment/checkout", &target{Namespace: testNamespace, Kind: testKind, Name: testCheckout}, false},
		{"Deployment//checkout", nil, true},
		{"a/b/c/d", nil, true},
	}
	for _, testCase := range cases {
		t.Run(testCase.input, func(t *testing.T) {
			got, err := parseTarget(testCase.input)
			if testCase.wantErr {
				if err == nil {
					t.Fatalf("parseTarget(%q) = %+v, want error", testCase.input, got)
				}
				requireCode(t, err, ExitUsage)
				return
			}
			if err != nil {
				t.Fatalf("parseTarget(%q): %v", testCase.input, err)
			}
			if testCase.want == nil {
				if got != nil {
					t.Fatalf("parseTarget(%q) = %+v, want nil", testCase.input, got)
				}
				return
			}
			if *got != *testCase.want {
				t.Fatalf("parseTarget(%q) = %+v, want %+v", testCase.input, got, testCase.want)
			}
		})
	}
}

func TestRequestRejectsContradictoryNamespaces(t *testing.T) {
	instance := &app{opts: &options{namespace: testNamespace, targetHint: "other/Deployment/checkout"}}
	_, err := instance.requestFor("why?")
	requireCode(t, err, ExitUsage)
	containsAll(t, ErrorText(err), "contradicts")
}

func TestSanitizeEscapesTerminalControlText(t *testing.T) {
	got := sanitize("a\x1b[2Jb\u202ec\rd\x7f")
	containsAll(t, got, `a\x1b[2Jb`, `\u202e`, `\x0d`, `\x7f`)
	excludesAll(t, got, "\x1b", "\u202e", "\r")
}

func TestSanitizeKeepsNewlinesAndTabs(t *testing.T) {
	if got := sanitize("one\ntwo\tthree"); got != "one\ntwo\tthree" {
		t.Fatalf("sanitize = %q", got)
	}
}

func TestTruncateDoesNotSplitARune(t *testing.T) {
	if got := truncate("aé", 2); got != "a..." {
		t.Fatalf("truncate = %q", got)
	}
}
