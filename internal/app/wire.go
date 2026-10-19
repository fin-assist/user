package app

import (
	"github.com/fin-assist/user/internal/adapter/grpc/server"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/worker"
	"github.com/fin-assist/user/migrations"
)

// AccessPolicy is the table «method → callers → model» of this service, copied from
// grpc-api.md 3.3 (the source of truth). The server refuses to start while a registered
// method has no rule. For example, in user:
//
//	"/finassist.user.v1.ProfileService/*":                              {Callers: []string{"bank-bff"}, Model: server.ModelUser},
//	"/finassist.user.v1.AuthService/*":                                 {Callers: []string{"bank-bff"}, Model: server.ModelNoUser},
//	"/finassist.user.v1.AuthService/Logout":                            {Callers: []string{"bank-bff"}, Model: server.ModelUser},
//	"/finassist.user.v1.UserInternalService/GetAssistantConsentStatus": {Callers: []string{"assistant"}, Model: server.ModelMixed},
//	"/finassist.user.v1.UserInternalService/GetSigningKeys":            {Callers: []string{server.AnyCaller}, Model: server.ModelNoUser},
func AccessPolicy() server.Policy {
	return server.Policy{}
}

// MigrationSources lists the migration sources in the order they are applied: libraries
// first (each with its own version table), then the service. A library exposes its files
// and table; add it here when the service starts using it, for example:
//
//	{Name: outbox.MigrationsName, Table: outbox.MigrationsTable, FS: outbox.Migrations},
func MigrationSources() []migrate.Source {
	return []migrate.Source{
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire builds repositories, clients, usecases and handlers, registers the handlers on
// a.GRPC.GRPC() and returns the background jobs and a function that closes what it
// opened (client connections). The order is adapters → usecases → delivery:
//
//	conn, err := client.Dial(a.Cfg.Peers["analytics"], client.Options{
//		Peer: "analytics", Tokens: a.Tokens, Metrics: a.Clients,
//		Retry: []string{"finassist.analytics.v1.AnalyticsService/GetAnalytics"},
//	})
//	repo := postgres.NewOperations(a.DB)                      // internal/adapter/postgres
//	uc := operations.New(a.DB, repo, analytics.New(conn))     // internal/operations
//	operationsgrpc.Register(a.GRPC.GRPC(), uc)                // internal/adapter/grpc/operations
//	jobs = append(jobs, outbox.Dispatcher(a.DB, ...))         // FIN-109
func wire(a *App) (jobs []worker.Job, closeAll func(), err error) {
	return nil, func() {}, nil
}
