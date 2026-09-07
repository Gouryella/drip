package utils

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestQueryKeysForLogOmitsValuesAndSorts(t *testing.T) {
	u, err := url.Parse("/callback?token=secret-token&visible=plain&Token=other&empty=")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	got := QueryKeysForLog(u)
	want := []string{"empty", "token", "visible"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("QueryKeysForLog() = %v, want %v", got, want)
	}

	joined := strings.Join(got, ",")
	if strings.Contains(joined, "secret-token") || strings.Contains(joined, "plain") {
		t.Fatalf("query keys exposed a query value: %q", joined)
	}
}

func TestRedactedRawQueryForLogRedactsSensitiveValues(t *testing.T) {
	u, err := url.Parse("/callback?token=secret-token&password=hunter2&visible=plain")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	raw := RedactedRawQueryForLog(u)
	if strings.Contains(raw, "secret-token") || strings.Contains(raw, "hunter2") {
		t.Fatalf("redacted query leaked sensitive value: %q", raw)
	}

	values, err := url.ParseQuery(raw)
	if err != nil {
		t.Fatalf("parse redacted query: %v", err)
	}
	if got := values.Get("token"); got != RedactedLogValue {
		t.Fatalf("token = %q, want redacted marker", got)
	}
	if got := values.Get("password"); got != RedactedLogValue {
		t.Fatalf("password = %q, want redacted marker", got)
	}
	if got := values.Get("visible"); got != "plain" {
		t.Fatalf("visible = %q, want plain", got)
	}
}

func TestURLPathForLogOmitsRawQuery(t *testing.T) {
	u, err := url.Parse("/private/resource?token=secret-token")
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}

	got := URLPathForLog(u)
	if got != "/private/resource" {
		t.Fatalf("URLPathForLog() = %q, want path only", got)
	}
	if strings.Contains(got, "secret-token") || strings.Contains(got, "?") {
		t.Fatalf("path log value included raw query: %q", got)
	}
}
