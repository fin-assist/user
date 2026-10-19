package config

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the generated tables of docs/config.md")

const docPath = "../../docs/config.md"

// The tables of docs/config.md are generated from Vars. CI fails when they are stale;
// `go test ./internal/config -run TestDocIsCurrent -update` rewrites them.
func TestDocIsCurrent(t *testing.T) {
	b, err := os.ReadFile(docPath)
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	got := doc
	for _, command := range []string{"serve", "migrate"} {
		got, err = replaceBlock(got, "config-"+command, Table(command))
		if err != nil {
			t.Fatal(err)
		}
	}
	if got == doc {
		return
	}
	if *update {
		if err := os.WriteFile(docPath, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	t.Fatalf("%s is stale: run go test ./internal/config -run TestDocIsCurrent -update", filepath.Clean(docPath))
}

func replaceBlock(doc, name, body string) (string, error) {
	start := fmt.Sprintf("<!-- generated:%s:start (go test ./internal/config -run TestDocIsCurrent -update) -->\n", name)
	end := fmt.Sprintf("<!-- generated:%s:end -->", name)
	i, j := strings.Index(doc, start), strings.Index(doc, end)
	if i < 0 || j < i {
		return "", fmt.Errorf("docs/config.md has no block %q (markers %q … %q)", name, strings.TrimSpace(start), end)
	}
	return doc[:i+len(start)] + body + doc[j:], nil
}

// Every declared variable is read by its command and every read one is declared, so the
// table lists exactly what the binary reads.
func TestVarsMatchWhatLoadReads(t *testing.T) {
	read := map[string]bool{}
	onRead = func(command, name string) { read[command+" "+name] = true }
	t.Cleanup(func() { onRead = nil })

	get, list := env(base())
	if _, err := load(get, noFiles, list); err != nil {
		t.Fatal(err)
	}
	if _, err := loadMigrate(get, noFiles); err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	for _, v := range Vars {
		key := v.Command + " " + v.Name
		if declared[key] {
			t.Errorf("%s is declared twice", key)
		}
		declared[key] = true
		if !read[key] {
			t.Errorf("%s is declared but never read: remove it or read it", key)
		}
	}
	for key := range read {
		if !declared[key] {
			t.Errorf("%s is read but not declared", key)
		}
	}
}

func TestUndeclaredVariableIsAnError(t *testing.T) {
	r := reader{getenv: func(string) string { return "" }, command: "serve"}
	r.str("NOT_DECLARED")
	r.secret("GRPC_ADDR") // declared, but not as a secret
	if len(r.errs) != 2 {
		t.Fatalf("errors %v", r.errs)
	}
}

func TestLoadMigrate(t *testing.T) {
	get, _ := env(map[string]string{"DB_PRIMARY_DSN": "postgres://p/db", "MIGRATE_WAIT_TIMEOUT": "1h"})
	c, err := loadMigrate(get, noFiles)
	if err != nil || c.LockTimeout.String() != "5s" || c.StatementTimeout.String() != "15m0s" || c.WaitTimeout.String() != "1h0m0s" {
		t.Fatalf("%+v %v", c, err)
	}
	get, _ = env(map[string]string{"MIGRATE_LOCK_TIMEOUT": "0s"})
	_, err = loadMigrate(get, noFiles)
	if err == nil || !strings.Contains(err.Error(), "MIGRATE_LOCK_TIMEOUT") || !strings.Contains(err.Error(), "DB_PRIMARY_DSN") {
		t.Fatalf("error %v", err)
	}
}
