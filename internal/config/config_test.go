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

func TestUnitDefaults(t *testing.T) {
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

func TestUnitSecretFromFile(t *testing.T) {
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

func TestUnitSecretBothSetIsRejected(t *testing.T) {
	vars := base()
	vars["DB_PRIMARY_DSN_FILE"] = "/x"
	get, list := env(vars)
	if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), "both DB_PRIMARY_DSN") {
		t.Fatalf("error %v", err)
	}
}

func TestUnitSecretFileErrorDoesNotLeakContent(t *testing.T) {
	vars := base()
	delete(vars, "DB_PRIMARY_DSN")
	vars["DB_PRIMARY_DSN_FILE"] = "/missing"
	get, list := env(vars)
	_, err := load(get, noFiles, list)
	if err == nil || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %v", err)
	}
}

func TestUnitInsecureDevOnlyInDev(t *testing.T) {
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

func TestUnitServiceAuthenticationFollowsEnvironment(t *testing.T) {
	for _, tt := range []struct {
		name  string
		vars  map[string]string
		valid bool
	}{
		{"prod defaults", map[string]string{"ENV": "prod"}, true},
		{"stage", map[string]string{"ENV": "stage", "AUTH_STAGE_ISSUER": "stage", "AUTH_STAGE_SIGNING_KEY_FILE": "/stage.pem", "AUTH_STAGE_PUBLIC_KEYS_FILE": "/stage.jwks"}, true},
		{"verify in dev", map[string]string{"ENV": "dev"}, false},
		{"stage keys in prod", map[string]string{"ENV": "prod", "AUTH_STAGE_PUBLIC_KEYS_FILE": "/stage.jwks"}, false},
	} {
		vars := base()
		for k, v := range tt.vars {
			vars[k] = v
		}
		get, list := env(vars)
		c, err := load(get, noFiles, list)
		if (err == nil) != tt.valid {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if tt.name == "prod defaults" {
			want := KubernetesAuthConfig{Issuer: "https://kubernetes.default.svc.cluster.local", JWKSURL: "https://kubernetes.default.svc/openid/v1/jwks", CAFile: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", APITokenFile: "/var/run/secrets/kubernetes.io/serviceaccount/token", Namespace: "finassist"}
			if c.Auth.Kubernetes != want {
				t.Fatalf("kubernetes defaults: %+v", c.Auth.Kubernetes)
			}
		}
	}
}

// FIN-347: with AUTH_MODE=verify and ENV=stage every AUTH_STAGE_* setting is required by
// the configuration itself, each missing one named, as in the other services.
func TestUnitStageIdentityRequiresEveryStageSetting(t *testing.T) {
	complete := map[string]string{"AUTH_STAGE_ISSUER": "stage-services", "AUTH_STAGE_SIGNING_KEY_FILE": "/stage.pem", "AUTH_STAGE_PUBLIC_KEYS_FILE": "/stage.jwks"}
	for name := range complete {
		for _, value := range []string{"", "  "} {
			vars := base()
			vars["ENV"] = "stage"
			for k, v := range complete {
				vars[k] = v
			}
			vars[name] = value
			get, list := env(vars)
			_, err := load(get, noFiles, list)
			if err == nil || !strings.Contains(err.Error(), name+" is required with AUTH_MODE=verify and ENV=stage") {
				t.Fatalf("%s=%q: %v", name, value, err)
			}
			for other := range complete {
				if other != name && strings.Contains(err.Error(), other) {
					t.Fatalf("%s=%q also reported the set %s: %v", name, value, other, err)
				}
			}
		}
	}
	vars := base()
	vars["ENV"] = "stage"
	get, list := env(vars)
	_, err := load(get, noFiles, list)
	for name := range complete {
		if err == nil || !strings.Contains(err.Error(), name+" is required") {
			t.Fatalf("all stage settings missing, %s not reported: %v", name, err)
		}
	}
	// prod and dev do not require stage settings; insecure-dev in dev never reads them.
	for _, e := range []map[string]string{{"ENV": "prod"}, {"ENV": "dev", "AUTH_MODE": "insecure-dev"}} {
		vars := base()
		for k, v := range e {
			vars[k] = v
		}
		get, list := env(vars)
		if _, err := load(get, noFiles, list); err != nil {
			t.Fatalf("%v: %v", e, err)
		}
	}
}

// FIN-325: a deployment that still sets a renamed variable fails at startup and is
// told the replacement, instead of silently running on the new variable's default.
func TestUnitRenamedIdentityVariablesAreRefused(t *testing.T) {
	for _, v := range renamedVars {
		vars := base()
		vars[v.old] = "legacy"
		get, list := env(vars)
		if _, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), v.old+" is no longer read: use "+v.replacement) {
			t.Fatalf("%s: %v", v.old, err)
		}
	}
	vars := base()
	vars["KUBERNETES_JWKS_URL"] = ""
	get, list := env(vars)
	if _, err := load(get, noFiles, list); err != nil {
		t.Fatalf("an empty legacy variable is not a setting: %v", err)
	}
}

func TestUnitPeers(t *testing.T) {
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

func TestUnitErrorsAreJoined(t *testing.T) {
	get, list := env(map[string]string{"ENV": "qa", "DB_MAX_CONNS": "x", "SHUTDOWN_DELAY": "-1s"})
	_, err := load(get, noFiles, list)
	for _, want := range []string{"SERVICE_NAME", "ENV=", "DB_MAX_CONNS", "SHUTDOWN_DELAY", "DB_PRIMARY_DSN"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error %v, want it to mention %s", err, want)
		}
	}
}

// A pool size beyond int32 used to wrap around (4294967297 became 1) and pass the
// positivity check; it is a configuration error instead.
func TestUnitMaxConnsOutsideInt32IsRejected(t *testing.T) {
	for _, v := range []string{"4294967297", "2147483648", "-2147483649", "99999999999999999999"} {
		vars := base()
		vars["DB_MAX_CONNS"] = v
		get, list := env(vars)
		if c, err := load(get, noFiles, list); err == nil || !strings.Contains(err.Error(), "DB_MAX_CONNS") {
			t.Fatalf("DB_MAX_CONNS=%s accepted as %d: %v", v, c.DB.MaxConns, err)
		}
	}
	vars := base()
	vars["DB_MAX_CONNS"] = "2147483647"
	get, list := env(vars)
	if c, err := load(get, noFiles, list); err != nil || c.DB.MaxConns != 2147483647 {
		t.Fatalf("max int32: %d %v", c.DB.MaxConns, err)
	}
}
