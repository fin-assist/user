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
		Doc: "`verify` — проверка JWT и токенов ServiceAccount (FIN-119, пока не подключена: сервис не стартует); `insecure-dev` — токены `dev:<имя>` без проверки, только при `ENV=dev`"},
	{Name: "SERVICE_TOKEN_FILE", Command: "serve",
		Doc: "файл с токеном ServiceAccount этого сервиса для исходящих вызовов (FIN-119)"},
	{Name: "DB_PRIMARY_DSN", Command: "serve", Required: true, Secret: true,
		Doc: "DSN primary (`-rw`)"},
	{Name: "DB_REPLICA_DSN", Command: "serve", Secret: true,
		Doc: "DSN реплик (`-ro`); пусто — чтение тоже с primary. На стенде оба указывают на один PostgreSQL"},
	{Name: "DB_MAX_CONNS", Command: "serve", Default: "10",
		Doc: "размер каждого пула"},
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
