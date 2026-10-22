package main

import (
	"reflect"
	"testing"
)

func TestParseKindArgs(t *testing.T) {
	got := parseKindArgs(hostList{"pi=--auto; claude=--model opus", "", "pi= --verbose ", "codex=--tools a,b"})
	want := map[string][]string{
		"pi":     {"--auto", "--verbose"},
		"claude": {"--model", "opus"},
		"codex":  {"--tools", "a,b"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseKindArgs = %v, want %v", got, want)
	}
	if got := parseKindArgs(hostList{""}); len(got) != 0 {
		t.Fatalf("empty = %v", got)
	}
}
