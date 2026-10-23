package config

import (
	"fmt"
	"strings"
)

// Var declares one environment variable: its default and its description in docs/config.md.
// Every variable the binary reads is declared here; reading an undeclared one is a
// configuration error, and the tables of docs/config.md are generated from this list
// (TestDocIsCurrent), so the documentation cannot drift from the code.
type Var struct {
	Name     string
	Command  string // serve or migrate: the table of docs/config.md it belongs to
	Default  string // as it would be written in the environment; empty = no default
	Required bool
	Secret   bool   // may also come from the file named by Name_FILE
	Doc      string // Russian, Markdown; shown in docs/config.md
}

// Vars lists every variable in the order of docs/config.md.
var Vars = []Var{
	{Name: "SERVICE_NAME", Command: "serve", Required: true,
		Doc: "имя сервиса: логи, `application_name`, токен в dev"},
	{Name: "ENV", Command: "serve", Default: string(EnvProd),
		Doc: "`dev`, `stage` или `prod`. Reflection включён только при `dev`"},
	{Name: "AUTH_MODE", Command: "serve", Default: string(AuthVerify),
		Doc: "`verify` — проверка JWT и сервисных токенов через go-identity; `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev`"},
	{Name: "JWT_SIGNING_KEY_FILE", Command: "serve", Default: "", Doc: "PKCS#8 PEM Ed25519, приватный ключ access JWT; файл 0600, обязателен при старте"},
	{Name: "JWT_PUBLIC_KEYS_FILE", Command: "serve", Default: "", Doc: "необязательный публичный JWKS Ed25519 для перекрытия ключей при ротации"},
	{Name: "JWT_KEY_ID", Command: "serve", Default: "", Doc: "kid активного ключа access JWT, обязателен при старте"},
	{Name: "JWT_ISSUER", Command: "serve", Default: "", Doc: "iss access JWT, обязателен; одинаков у всех потребителей"},
	{Name: "JWT_AUDIENCE", Command: "serve", Default: "", Doc: "aud access JWT, обязателен; одинаков у всех потребителей"},
	{Name: "AUTH_IDEMPOTENCY_KEY_FILE", Command: "serve", Default: "", Doc: "файл 0600 с 32 raw-байтами AES-256 для шифрования ответов Register/Refresh; обязателен, общий для реплик; сохранять ключ не менее 24 ч после последней записи"},
	{Name: "AUTH_DB_MAX_CONNS", Command: "serve", Default: "8", Doc: "отдельный primary pool транзакций авторизации; лимиты используют обычный pool, чтобы ошибки токена не откатывали учёт попыток"},
	{Name: "REFRESH_TOKEN_TTL", Command: "serve", Default: "1440h", Doc: "скользящий срок refresh-токена; по умолчанию 60 суток, продлевается каждой ротацией"},
	{Name: "SERVICE_AUTH_MODE", Command: "serve", Default: "kubernetes", Doc: "kubernetes — ServiceAccount; stage — подписанные JWT сервисов, только dev/stage"},
	{Name: "SERVICE_TOKEN_ISSUER", Command: "serve", Default: "", Doc: "iss сервисных токенов; для kubernetes — issuer кластера, для stage — общий issuer стенда"},
	{Name: "STAGE_SIGNING_KEY_FILE", Command: "serve", Default: "", Doc: "приватный PKCS#8 Ed25519 ключ этого сервиса для SERVICE_AUTH_MODE=stage"},
	{Name: "STAGE_PUBLIC_KEYS_FILE", Command: "serve", Default: "", Doc: "публичный JWKS сервисов стенда; kid — имя сервиса; отдельный от ключей пользователей"},
	{Name: "KUBERNETES_JWKS_URL", Command: "serve", Default: "https://kubernetes.default.svc/openid/v1/jwks", Doc: "доверенный HTTPS endpoint JWKS API-сервера"},
	{Name: "KUBERNETES_CA_FILE", Command: "serve", Default: "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", Doc: "доверенный CA API-сервера, TLS-проверка обязательна"},
	{Name: "KUBERNETES_TOKEN_FILE", Command: "serve", Default: "/var/run/secrets/kubernetes.io/serviceaccount/token", Doc: "штатный API-server-audience токен пода для чтения JWKS; не finassist-internal"},
	{Name: "SERVICE_TOKEN_FILE", Command: "serve",
		Doc: "файл с токеном ServiceAccount этого сервиса для исходящих вызовов (FIN-119)"},
	{Name: "DB_PRIMARY_DSN", Command: "serve", Required: true, Secret: true,
		Doc: "DSN primary (`-rw`)"},
	{Name: "DB_REPLICA_DSN", Command: "serve", Secret: true,
		Doc: "DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL"},
	{Name: "DB_MAX_CONNS", Command: "serve", Default: "10",
		Doc: "размер каждого пула"},
	{Name: "OUTBOX_TIMEZONE_TARGETS", Command: "serve", Default: "bank-statement-data,assistant", Doc: "получатели user.timezone_changed через запятую без пробелов; каждому нужен PEER_<NAME>_ADDR; 1–64 уникальных имени"},
	{Name: "OUTBOX_DELETION_TARGETS", Command: "serve", Default: "bank-statement-data,analytics,assistant", Doc: "получатели user.deleted; обязательны bank-statement-data, analytics, assistant; каждому нужен PEER_<NAME>_ADDR; дополнительные получатели допустимы"},
	{Name: "OUTBOX_DB_MAX_CONNS", Command: "serve", Default: "8", Doc: "размер отдельного primary-пула доставщика, не меньше числа уникальных получателей обоих типов событий + 3"},
	{Name: "OUTBOX_POLL_INTERVAL", Command: "serve", Default: "3s", Doc: "запасной опрос outbox при отсутствии уведомлений, положительный интервал"},
	{Name: "LIMIT_VERIFICATION_PHONE_SHORT", Command: "serve", Default: "3", Doc: "StartPhoneVerification: предел короткого окна на телефон"},
	{Name: "WINDOW_VERIFICATION_PHONE_SHORT", Command: "serve", Default: "1h", Doc: "StartPhoneVerification: короткое UTC-окно на телефон"},
	{Name: "LIMIT_VERIFICATION_PHONE_LONG", Command: "serve", Default: "10", Doc: "StartPhoneVerification: предел длинного окна на телефон"},
	{Name: "WINDOW_VERIFICATION_PHONE_LONG", Command: "serve", Default: "24h", Doc: "StartPhoneVerification: длинное UTC-окно на телефон"},
	{Name: "LIMIT_VERIFICATION_IP_SHORT", Command: "serve", Default: "30", Doc: "StartPhoneVerification: предел короткого окна на IP"},
	{Name: "WINDOW_VERIFICATION_IP_SHORT", Command: "serve", Default: "1m", Doc: "StartPhoneVerification: короткое UTC-окно на IP"},
	{Name: "LIMIT_VERIFICATION_IP_LONG", Command: "serve", Default: "300", Doc: "StartPhoneVerification: предел длинного окна на IP"},
	{Name: "WINDOW_VERIFICATION_IP_LONG", Command: "serve", Default: "1h", Doc: "StartPhoneVerification: длинное UTC-окно на IP"},
	{Name: "LIMIT_WATCH_IP", Command: "serve", Default: "60", Doc: "WatchPhoneVerification: предел попыток открыть поток на IP, до проверки токена"},
	{Name: "WINDOW_WATCH_IP", Command: "serve", Default: "1m", Doc: "WatchPhoneVerification: UTC-окно попыток открыть поток"},
	{Name: "LIMIT_REGISTER_IP", Command: "serve", Default: "30", Doc: "Register: предел запросов на IP, после идемпотентности, до проверки токена"},
	{Name: "WINDOW_REGISTER_IP", Command: "serve", Default: "1m", Doc: "Register: UTC-окно запросов на IP"},
	{Name: "LIMIT_REFRESH_IP", Command: "serve", Default: "120", Doc: "RefreshTokens: предел запросов на IP, после идемпотентности, до проверки токена"},
	{Name: "WINDOW_REFRESH_IP", Command: "serve", Default: "1m", Doc: "RefreshTokens: UTC-окно запросов на IP"},
	{Name: "GRPC_ADDR", Command: "serve", Default: ":9090",
		Doc: "gRPC"},
	{Name: "GRPC_MAX_RECV_MSG_SIZE", Command: "serve", Default: "4194304",
		Doc: "байт; 16 МиБ там, где обслуживается `SearchOperations` (grpc-api.md 9)"},
	{Name: "HTTP_ADDR", Command: "serve", Default: ":9091",
		Doc: "`/metrics`, `/healthz`, `/readyz`"},
	{Name: "PEER_<NAME>_ADDR", Command: "serve",
		Doc: "адрес другого сервиса: `PEER_BANK_STATEMENT_DATA_ADDR` → `cfg.Peers[\"bank-statement-data\"]`. k3s: `dns:///bank-statement-data.finassist.svc.cluster.local:9090`; стенд: `dns:///127.0.0.1:9092`"},
	{Name: "LOG_LEVEL", Command: "serve", Default: "info",
		Doc: "`debug` добавляет в строку вызова metadata и запрос с замаскированными токенами"},
	{Name: "LOG_FORMAT", Command: "serve", Default: "json",
		Doc: "`json` или `text`"},
	{Name: "SHUTDOWN_DELAY", Command: "serve", Default: "5s",
		Doc: "после `NOT_SERVING` сервер ещё принимает вызовы, пока балансировщики не уберут под"},
	{Name: "SHUTDOWN_TIMEOUT", Command: "serve", Default: "20s",
		Doc: "сколько ждать завершения вызовов; оставшиеся обрываются (потоки ответа клиент переоткроет)"},

	{Name: "DB_PRIMARY_DSN", Command: "migrate", Required: true, Secret: true,
		Doc: "DSN primary, роль сервиса. Миграции никогда не идут на реплику"},
	{Name: "MIGRATE_LOCK_TIMEOUT", Command: "migrate", Default: "5s",
		Doc: "`lock_timeout`: DDL, который не дождался блокировки занятой таблицы, падает, а не держит очередь запросов сервиса за собой"},
	{Name: "MIGRATE_STATEMENT_TIMEOUT", Command: "migrate", Default: "15m",
		Doc: "`statement_timeout` каждой команды"},
	{Name: "MIGRATE_WAIT_TIMEOUT", Command: "migrate", Default: "15m",
		Doc: "сколько ждать, пока другой запуск миграций того же источника закончит"},
}

// Table renders the Markdown table of one command for docs/config.md.
func Table(command string) string {
	var b strings.Builder
	b.WriteString("| Переменная | По умолчанию | Что |\n|---|---|---|\n")
	for _, v := range Vars {
		if v.Command != command {
			continue
		}
		name := "`" + v.Name + "`"
		if v.Secret {
			name += " / `_FILE`"
		}
		def := "—"
		switch {
		case v.Default != "":
			def = "`" + v.Default + "`"
		case v.Required:
			def = "— (обязательна)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", name, def, v.Doc)
	}
	return b.String()
}
