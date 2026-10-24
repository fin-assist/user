package client

import (
	"path/filepath"
	"testing"
)

func TestUnitTokenErrors(t *testing.T) {
	f := &FileToken{Path: filepath.Join(t.TempDir(), "missing")}
	if _, err := f.Token(t.Context()); err == nil {
		t.Fatal("a missing token file must be an error")
	}
	if _, err := DevToken("").Token(t.Context()); err == nil {
		t.Fatal("an empty dev token must be an error")
	}
	if tok, err := DevToken("assistant").Token(t.Context()); err != nil || tok != "dev:assistant" {
		t.Fatalf("%q %v", tok, err)
	}
}

func TestUnitDialRequiresPeerAndTokens(t *testing.T) {
	if _, err := Dial("dns:///x:9090", Options{Peer: "x"}); err == nil {
		t.Fatal("Dial without Tokens must fail")
	}
	if _, err := Dial("dns:///x:9090", Options{Tokens: DevToken("a"), Retry: []string{"/bad"}}); err == nil {
		t.Fatal("Dial without Peer must fail")
	}
	if _, err := Dial("dns:///x:9090", Options{Peer: "x", Tokens: DevToken("a"), Retry: []string{"/bad"}}); err == nil {
		t.Fatal("a bad retry entry must fail Dial")
	}
}
