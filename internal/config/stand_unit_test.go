package config

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	goidentity "github.com/fin-assist/go-identity"

	"github.com/fin-assist/user/internal/signing"
)

// standService is the one source of the service name in the stand unit: its paths, its
// SERVICE_NAME and the kid of its stand signing key all derive from it.
const standService = "user"

var (
	standUnitPath  = filepath.Join("..", "..", "deploy", "systemd", standService+".service")
	standConfigDir = "/etc/finassist/" + standService
	standEnvFile   = standConfigDir + "/env"
	standJWKS      = "/etc/finassist/stage-public.jwks"
	envName        = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)
	credentialName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	peerAddr       = regexp.MustCompile(`^PEER_[A-Z0-9_]+_ADDR$`)
)

// standSecretFiles are the *_FILE variables of the unit that name private material: the
// _FILE form of every secret in Vars plus the key-file settings read only as files.
func standSecretFiles() map[string]bool {
	files := map[string]bool{"JWT_SIGNING_KEY_FILE": true, "AUTH_IDEMPOTENCY_KEY_FILE": true, "PHONE_RESULT_KEY_FILE": true, "AUTH_STAGE_SIGNING_KEY_FILE": true}
	for _, v := range Vars {
		if v.Command == "serve" && v.Secret {
			files[v.Name+"_FILE"] = true
		}
	}
	return files
}

// standRequiredFiles are the private files the stage configuration cannot start without.
var standRequiredFiles = []string{"DB_PRIMARY_DSN_FILE", "JWT_SIGNING_KEY_FILE", "AUTH_IDEMPOTENCY_KEY_FILE", "PHONE_RESULT_KEY_FILE", "AUTH_STAGE_SIGNING_KEY_FILE"}

// standUnit is what the unit gives the process: Environment= settings with %d kept
// as written, and the LoadCredential= names with their sources.
type standUnit struct {
	env         map[string]string
	credentials map[string]string
}

// allowedEnv reports why name may not appear in Environment=, or "" when it may: a
// non-secret variable of Vars, the _FILE form of a secret, a key-file setting or a peer.
func allowedEnv(name string, secretFiles map[string]bool) string {
	if secretFiles[name] || peerAddr.MatchString(name) {
		return ""
	}
	for _, v := range Vars {
		if v.Command != "serve" || v.Name != name {
			continue
		}
		if v.Secret {
			return "is a secret: pass it as " + name + "_FILE=%d/<credential>"
		}
		return ""
	}
	return "is not a variable of the serve configuration"
}

// checkStandUnit parses the unit text and checks it against what the stage configuration
// of the service needs under DynamicUser=yes. Only directives of [Service] count.
func checkStandUnit(text string) (standUnit, error) {
	unit := standUnit{env: map[string]string{}, credentials: map[string]string{}}
	secretFiles := standSecretFiles()
	var envFiles []string
	section := ""
	scanner := bufio.NewScanner(strings.NewReader(text))
	for n := 1; scanner.Scan(); n++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			return unit, fmt.Errorf("line %d: not a key=value directive", n)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		switch key {
		case "Environment", "EnvironmentFile", "LoadCredential", "LoadCredentialEncrypted", "SetCredential", "SetCredentialEncrypted", "ImportCredential":
			if section != "[Service]" {
				return unit, fmt.Errorf("line %d: %s outside [Service]", n, key)
			}
		}
		if section != "[Service]" {
			continue
		}
		switch key {
		case "Environment":
			if value == "" || strings.ContainsAny(value, "\"' \t\\") {
				return unit, fmt.Errorf("line %d: Environment= must be the plain form NAME=value, without quotes, spaces, tabs or backslashes", n)
			}
			name, setting, ok := strings.Cut(value, "=")
			if !ok || !envName.MatchString(name) {
				return unit, fmt.Errorf("line %d: Environment= must be the plain form NAME=value", n)
			}
			if reason := allowedEnv(name, secretFiles); reason != "" {
				return unit, fmt.Errorf("line %d: %s %s", n, name, reason)
			}
			if _, dup := unit.env[name]; dup {
				return unit, fmt.Errorf("line %d: %s is set twice", n, name)
			}
			unit.env[name] = setting
		case "EnvironmentFile":
			envFiles = append(envFiles, value)
		case "LoadCredential":
			name, source, ok := strings.Cut(value, ":")
			if !ok || !credentialName.MatchString(name) || source == "" {
				return unit, fmt.Errorf("line %d: LoadCredential= must be <name>:<source>", n)
			}
			source = filepath.Clean(source)
			if filepath.Dir(source) != standConfigDir {
				return unit, fmt.Errorf("line %d: credential %s source must lie directly in %s", n, name, standConfigDir)
			}
			if base := filepath.Base(source); base != name && strings.TrimSuffix(base, filepath.Ext(base)) != name {
				return unit, fmt.Errorf("line %d: credential %s source file name must be the credential name", n, name)
			}
			if _, dup := unit.credentials[name]; dup {
				return unit, fmt.Errorf("line %d: credential %s is loaded twice", n, name)
			}
			unit.credentials[name] = source
		case "SetCredential", "SetCredentialEncrypted":
			// A secret written into the unit is visible to anyone who can read it.
			return unit, fmt.Errorf("line %d: %s is not used: secrets come from files by LoadCredential=", n, key)
		case "LoadCredentialEncrypted", "ImportCredential":
			// Other credential sources are outside the checked layout of /etc/finassist/<service>.
			return unit, fmt.Errorf("line %d: %s is not used: secrets come from files by LoadCredential=", n, key)
		}
	}
	if err := scanner.Err(); err != nil {
		return unit, err
	}
	if len(envFiles) != 1 || envFiles[0] != standEnvFile {
		return unit, fmt.Errorf("EnvironmentFile= must be given once as %s, got %q", standEnvFile, envFiles)
	}
	if unit.env["ENV"] != "stage" {
		return unit, fmt.Errorf("ENV=%q, want stage", unit.env["ENV"])
	}
	if unit.env["SERVICE_NAME"] != standService {
		return unit, fmt.Errorf("SERVICE_NAME=%q, want %s (kid/sub of the stand signing key)", unit.env["SERVICE_NAME"], standService)
	}
	if unit.env["AUTH_STAGE_PUBLIC_KEYS_FILE"] != standJWKS {
		return unit, fmt.Errorf("AUTH_STAGE_PUBLIC_KEYS_FILE=%q, want the shared %s", unit.env["AUTH_STAGE_PUBLIC_KEYS_FILE"], standJWKS)
	}
	for _, name := range standRequiredFiles {
		if _, ok := unit.env[name]; !ok {
			return unit, fmt.Errorf("%s is required with ENV=stage", name)
		}
	}
	used := map[string]bool{}
	for name, setting := range unit.env {
		credential, viaCredential := strings.CutPrefix(setting, "%d/")
		if secretFiles[name] && !viaCredential {
			return unit, fmt.Errorf("%s=%q: a private file must be %%d/<credential>, a root-owned 0600 file is unreadable to DynamicUser", name, setting)
		}
		if !viaCredential {
			if strings.Contains(setting, "%d") {
				return unit, fmt.Errorf("%s=%q: %%d only as %%d/<credential>", name, setting)
			}
			continue
		}
		if _, ok := unit.credentials[credential]; !ok {
			return unit, fmt.Errorf("%s names credential %q that the unit does not load", name, credential)
		}
		used[credential] = true
	}
	for credential := range unit.credentials {
		if !used[credential] {
			return unit, fmt.Errorf("credential %q is loaded but no variable reads it", credential)
		}
	}
	return unit, nil
}

func readStandUnitText(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(standUnitPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUnitStandUnitPassesChecks(t *testing.T) {
	if _, err := checkStandUnit(readStandUnitText(t)); err != nil {
		t.Fatal(err)
	}
}

// Every rule of checkStandUnit refuses its mutation of the real unit; the positive
// cases are forms systemd accepts with the same meaning.
func TestUnitStandUnitChecksRefuseMutations(t *testing.T) {
	text := readStandUnitText(t)
	const (
		dynamicUser = "DynamicUser=yes\n"
		unitSection = "[Unit]\n"
	)
	credential := func(name, file string) string {
		return "LoadCredential=" + name + ":" + standConfigDir + "/" + file + "\n"
	}
	setting := func(name, value string) string { return "Environment=" + name + "=" + value + "\n" }
	var (
		authCredential   = credential("auth-idempotency", "auth-idempotency.key")
		authEnv          = setting("AUTH_IDEMPOTENCY_KEY_FILE", "%d/auth-idempotency")
		stageEnv         = setting("AUTH_STAGE_SIGNING_KEY_FILE", "%d/stage-signing")
		envLine          = setting("ENV", "stage")
		serviceLine      = setting("SERVICE_NAME", standService)
		envFileLine      = "EnvironmentFile=" + standEnvFile + "\n"
		jwksLine         = setting("AUTH_STAGE_PUBLIC_KEYS_FILE", standJWKS)
		zvonokCampaign   = "#" + setting("ZVONOK_CAMPAIGN_ID", "<campaign ID>")
		zvonokKeyEnv     = setting("ZVONOK_PUBLIC_KEY_FILE", "%d/zvonok-public-key")
		zvonokCredential = credential("zvonok-public-key", "zvonok-public-key")
	)
	replace := func(old, replacement string) func(*testing.T) string {
		return func(t *testing.T) string {
			if !strings.Contains(text, old) {
				t.Fatalf("the unit no longer contains %q", old)
			}
			return strings.Replace(text, old, replacement, 1)
		}
	}
	add := func(lines string) func(*testing.T) string { return replace(dynamicUser, lines+dynamicUser) }
	// zvonok enables the commented Zvonok block of the unit as an operator would: every
	// "#LoadCredential=" and "#Environment=" line loses its "#", the campaign gets an ID.
	// edit changes the commented template before that, as a broken template would.
	zvonok := func(edit func(string) string) func(*testing.T) string {
		return func(t *testing.T) string {
			if !strings.Contains(text, zvonokCampaign) {
				t.Fatalf("the unit no longer contains %q", zvonokCampaign)
			}
			out := strings.Replace(edit(text), zvonokCampaign, setting("ZVONOK_CAMPAIGN_ID", "123456"), 1)
			lines := strings.SplitAfter(out, "\n")
			for i, line := range lines {
				if strings.HasPrefix(line, "#LoadCredential=") || strings.HasPrefix(line, "#Environment=") {
					lines[i] = line[1:]
				}
			}
			return strings.Join(lines, "")
		}
	}
	keep := func(s string) string { return s }
	editTemplate := func(old, replacement string) func(string) string {
		return func(s string) string { return strings.Replace(s, "#"+old, "#"+replacement, 1) }
	}
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T) string
		want   string // "" accepts
	}{
		{"no idempotency credential", replace(authCredential, ""), "does not load"},
		{"no idempotency variable", replace(authEnv, ""), "AUTH_IDEMPOTENCY_KEY_FILE is required"},
		{"idempotency key from /etc", replace(authEnv, setting("AUTH_IDEMPOTENCY_KEY_FILE", standConfigDir+"/auth-idempotency.key")), "must be %d/<credential>"},
		{"stand signing key from /etc", replace(stageEnv, setting("AUTH_STAGE_SIGNING_KEY_FILE", standConfigDir+"/stage-signing.pem")), "must be %d/<credential>"},
		{"quoted assignment", replace(envLine, "Environment=\"ENV=stage\"\n"), "plain form"},
		{"two assignments", replace(envLine, setting("ENV", "stage LOG_LEVEL=debug")), "plain form"},
		{"tab", replace(envLine, setting("ENV", "stage\tx")), "plain form"},
		{"backslash", replace(envLine, setting("ENV", "sta\\ge")), "plain form"},
		{"empty", add("Environment=\n"), "plain form"},
		{"lower-case name", add(setting("env", "stage")), "plain form"},
		{"variable outside the configuration", add(setting("PGPASSWORD", "secret")), "PGPASSWORD is not a variable"},
		{"secret value", add(setting("DB_REPLICA_DSN", "postgres://u:p@host/db")), "DB_REPLICA_DSN is a secret"},
		{"SetCredential", add("SetCredential=db-replica-dsn:postgres://u:p@host/db\n"), "SetCredential"},
		{"SetCredentialEncrypted", add("SetCredentialEncrypted=db-replica-dsn:AAAA\n"), "SetCredentialEncrypted"},
		{"LoadCredentialEncrypted", add("LoadCredentialEncrypted=db-replica-dsn:" + standConfigDir + "/db-replica-dsn.cred\n"), "LoadCredentialEncrypted"},
		{"ImportCredential", add("ImportCredential=db-replica-dsn\n"), "ImportCredential"},
		{"duplicate variable", add(setting("ENV", "prod")), "ENV is set twice"},
		{"wrong environment", replace(envLine, setting("ENV", "prod")), "want stage"},
		{"wrong service name", replace(serviceLine, setting("SERVICE_NAME", "analytics")), "want " + standService},
		{"no service name", replace(serviceLine, ""), "want " + standService},
		{"another JWKS path", replace(jwksLine, setting("AUTH_STAGE_PUBLIC_KEYS_FILE", standConfigDir+"/stage-public.jwks")), "shared"},
		{"credential source elsewhere", replace(authCredential, "LoadCredential=auth-idempotency:/etc/finassist/analytics/auth-idempotency.key\n"), "directly in " + standConfigDir},
		{"credential source escapes with ..", replace(authCredential, credential("auth-idempotency", "../auth-idempotency.key")), "directly in " + standConfigDir},
		{"credential source of another name", replace(authCredential, credential("auth-idempotency", "phone-result.key")), "file name must be the credential name"},
		{"credential without source", replace(authCredential, "LoadCredential=auth-idempotency\n"), "<name>:<source>"},
		{"credential loaded twice", add(authCredential), "loaded twice"},
		{"credential name reused for another source", add(credential("auth-idempotency", "auth-idempotency")), "loaded twice"},
		{"credential nobody reads", add(credential("spare", "spare.key")), "no variable reads it"},
		{"%d to an unloaded credential", add(setting("JWT_PUBLIC_KEYS_FILE", "%d/previous-public")), "does not load"},
		{"%d inside a path", add(setting("JWT_PUBLIC_KEYS_FILE", "/x%d/y")), "only as %d/<credential>"},
		{"no env file", replace(envFileLine, ""), "EnvironmentFile= must be given once"},
		{"second env file", add("EnvironmentFile=" + standEnvFile + ".extra\n"), "EnvironmentFile= must be given once"},
		{"optional env file", replace(envFileLine, "EnvironmentFile=-"+standEnvFile+"\n"), "EnvironmentFile= must be given once"},
		{"directive outside [Service]", replace(unitSection, unitSection+setting("LOG_LEVEL", "debug")), "outside [Service]"},
		{"credential outside [Service]", replace(unitSection, unitSection+authCredential), "outside [Service]"},
		{"spaces around =", replace(envLine, "Environment = ENV=stage\n"), ""},
		{"credential name with a dot", add(credential("zvonok.key", "zvonok.key") + setting("ZVONOK_PUBLIC_KEY_FILE", "%d/zvonok.key")), ""},
		{"new credential", add(credential("db-replica-dsn", "db-replica-dsn") + setting("DB_REPLICA_DSN_FILE", "%d/db-replica-dsn")), ""},
		{"peer address", add(setting("PEER_ANALYTICS_ADDR", "dns:///127.0.0.1:9093")), ""},
		{"Zvonok block enabled", zvonok(keep), ""},
		{"Zvonok key from /etc", zvonok(editTemplate(zvonokKeyEnv, setting("ZVONOK_PUBLIC_KEY_FILE", standConfigDir+"/zvonok-public-key"))), "must be %d/<credential>"},
		{"Zvonok block without its credential", zvonok(editTemplate(zvonokCredential, "")), "does not load"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := checkStandUnit(tc.mutate(t))
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("accepted form refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("got %v, want an error with %q", err, tc.want)
			}
		})
	}
}

// writeCredential places one credential as systemd does in $CREDENTIALS_DIRECTORY.
func writeCredential(t *testing.T, path string, content []byte, perm fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func ed25519PEM(t *testing.T) []byte {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}

// FIN-349: with ENV=stage and DynamicUser=yes the process reads only what the unit hands
// it. With credentials as systemd exposes them (0400 on ramfs, 0440 with the ACL mask on
// tmpfs) and an env file of only the settings the unit comments name, the configuration
// loads and every private key the service opens after load is accepted; a credential
// readable by others is still refused.
func TestUnitStandUnitMatchesStageConfiguration(t *testing.T) {
	unit, err := checkStandUnit(readStandUnitText(t))
	if err != nil {
		t.Fatal(err)
	}
	envFile := map[string]string{
		"JWT_KEY_ID": "stage-1", "JWT_ISSUER": "https://user.example", "JWT_AUDIENCE": "finassist",
		"AUTH_STAGE_ISSUER":             "stage-services",
		"PEER_BANK_STATEMENT_DATA_ADDR": "dns:///127.0.0.1:9092", "PEER_ANALYTICS_ADDR": "dns:///127.0.0.1:9093", "PEER_ASSISTANT_ADDR": "dns:///127.0.0.1:9094",
	}
	for _, tc := range []struct {
		perm fs.FileMode
		ok   bool
	}{{0o400, true}, {0o440, true}, {0o444, false}} {
		t.Run(fmt.Sprintf("%04o", tc.perm), func(t *testing.T) {
			credentials := t.TempDir()
			t.Setenv("CREDENTIALS_DIRECTORY", credentials)
			// Merge in systemd order: Environment= of the unit first, EnvironmentFile= over it.
			// An env file that repeated a unit setting would silently replace it, so refuse that.
			vars := map[string]string{}
			for name, value := range unit.env {
				vars[name] = strings.ReplaceAll(value, "%d", credentials)
			}
			for name, value := range envFile {
				if _, ok := unit.env[name]; ok {
					t.Fatalf("the env file would override the unit setting %s", name)
				}
				vars[name] = value
			}
			writeCredential(t, vars["DB_PRIMARY_DSN_FILE"], []byte("postgres://primary/user\n"), tc.perm)
			writeCredential(t, vars["AUTH_IDEMPOTENCY_KEY_FILE"], bytes.Repeat([]byte{1}, 32), tc.perm)
			writeCredential(t, vars["PHONE_RESULT_KEY_FILE"], bytes.Repeat([]byte{2}, 32), tc.perm)
			writeCredential(t, vars["JWT_SIGNING_KEY_FILE"], ed25519PEM(t), tc.perm)
			writeCredential(t, vars["AUTH_STAGE_SIGNING_KEY_FILE"], ed25519PEM(t), tc.perm)

			get, list := env(vars)
			c, err := load(get, os.ReadFile, list)
			if err != nil {
				t.Fatalf("stand unit configuration rejected: %v", err)
			}
			if c.Env != EnvStage || c.Auth.Mode != AuthVerify || c.Service != standService || c.DB.PrimaryDSN != "postgres://primary/user" {
				t.Fatalf("stand runs ENV=%s AUTH_MODE=%s SERVICE_NAME=%s", c.Env, c.Auth.Mode, c.Service)
			}
			_, sessionErr := LoadSessionKey(c.Sessions)
			_, phoneErr := LoadPhoneKey(c.Phone)
			_, signingErr := signing.Load(signing.Options{PrivateKeyFile: c.Signing.PrivateKeyFile, KeyID: c.Signing.KeyID, Issuer: c.Signing.Issuer, Audience: c.Signing.Audience, Clock: time.Now})
			keyErrs := errors.Join(sessionErr, phoneErr, signingErr)
			if !tc.ok {
				if sessionErr == nil || phoneErr == nil || signingErr == nil {
					t.Fatalf("credential readable by others accepted: %v", keyErrs)
				}
				return
			}
			if keyErrs != nil {
				t.Fatalf("private keys from the unit: %v", keyErrs)
			}
			// The stand signing key: go-identity reads it and signs with kid = SERVICE_NAME,
			// which must match the kid of this service in the shared JWKS.
			source, err := goidentity.NewStageSource(string(c.Env), goidentity.StageOptions{KeyFile: c.Auth.Stage.SigningKeyFile, Caller: c.Service, Issuer: c.Auth.Stage.Issuer, Clock: time.Now})
			if err != nil {
				t.Fatalf("AUTH_STAGE_SIGNING_KEY_FILE from the unit: %v", err)
			}
			token, err := source.Token(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			header, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[0])
			var h struct {
				Kid string `json:"kid"`
			}
			if err != nil || json.Unmarshal(header, &h) != nil || h.Kid != standService {
				t.Fatalf("stand token kid=%q, want %s", h.Kid, standService)
			}
		})
	}
}
