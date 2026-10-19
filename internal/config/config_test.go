package config

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func env(vars map[string]string) (func(string) string, func() []string) {
	get := func(k string) string { return vars[k] }
	list := func() []string {
		var out []string
		for k, v := range vars {
			out = append(out, k+"="+v)
		}
		return out
	}
	return get, list
}

func noFiles(string) ([]byte, error) { return nil, fs.ErrNotExist }

func base() map[string]string {
	return map[string]string{"SERVICE_NAME": "user", "DB_PRIMARY_DSN": "postgres://primary/user"}
}

func TestDefaults(t *testing.T) {
	get, list := env(base())
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	if c.Env != EnvProd || c.Auth.Mode != AuthVerify || c.GRPC.Reflection {
		t.Fatalf("unsafe defaults: env %s, auth %s, reflection %v", c.Env, c.Auth.Mode, c.GRPC.Reflection)
	}
	if c.GRPC.Addr != ":9090" || c.GRPC.MaxRecvMsgSize != 4<<20 || c.ShutdownTimeout != 20*time.Second {
		t.Fatalf("defaults: %+v", c)
	}
}

func TestSecretFromFile(t *testing.T) {
	vars := base()
	delete(vars, "DB_PRIMARY_DSN")
	vars["DB_PRIMARY_DSN_FILE"] = "/run/credentials/user/db"
	get, list := env(vars)
	read := func(p string) ([]byte, error) {
		if p == "/run/credentials/user/db" {
			return []byte("postgres://from-file/user\n"), nil
		}
		return nil, fs.ErrNotExist
	}
	c, err := load(get, read, list)
	if err != nil {
		t.Fatal(err)
	}
	if c.DB.PrimaryDSN != "postgres://from-file/user" {
		t.Fatalf("got %q", c.DB.PrimaryDSN)
	}
}

func TestSecretBothSetIsRejected(t *testing.T) {
	vars := base()
	vars["DB_PRIMARY_DSN_FILE"] = "/x"
	get, list := env(vars)
	if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), "both DB_PRIMARY_DSN") {
		t.Fatalf("error %v", err)
	}
}

func TestSecretFileErrorDoesNotLeakContent(t *testing.T) {
	vars := base()
	delete(vars, "DB_PRIMARY_DSN")
	vars["DB_PRIMARY_DSN_FILE"] = "/missing"
	get, list := env(vars)
	_, err := load(get, noFiles, list)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %v", err)
	}
}

func TestInsecureDevOnlyInDev(t *testing.T) {
	for _, e := range []string{"stage", "prod"} {
		vars := base()
		vars["ENV"], vars["AUTH_MODE"] = e, "insecure-dev"
		get, list := env(vars)
		if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), "only with ENV=dev") {
			t.Fatalf("%s: error %v", e, err)
		}
	}
	vars := base()
	vars["ENV"], vars["AUTH_MODE"] = "dev", "insecure-dev"
	get, list := env(vars)
	c, err := load(get, noFiles, list)
	if err != nil || !c.GRPC.Reflection {
		t.Fatalf("dev: %v, reflection %v", err, c.GRPC.Reflection)
	}
}

func TestPeers(t *testing.T) {
	vars := base()
	vars["PEER_BANK_STATEMENT_DATA_ADDR"] = "dns:///bank-statement-data.finassist.svc.cluster.local:9090"
	vars["PEER_ANALYTICS_ADDR"] = "127.0.0.1:9093"
	get, list := env(vars)
	c, err := load(get, noFiles, list)
	if err != nil {
		t.Fatal(err)
	}
	if c.Peers["bank-statement-data"] == "" || c.Peers["analytics"] != "127.0.0.1:9093" || len(c.Peers) != 2 {
		t.Fatalf("peers %v", c.Peers)
	}
}

func TestErrorsAreJoined(t *testing.T) {
	get, list := env(map[string]string{"ENV": "qa", "DB_MAX_CONNS": "x", "SHUTDOWN_DELAY": "-1s"})
	_, err := load(get, noFiles, list)
	for _, want := range []string{"SERVICE_NAME", "ENV=", "DB_MAX_CONNS", "SHUTDOWN_DELAY", "DB_PRIMARY_DSN"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v, want it to mention %s", err, want)
		}
	}
}
