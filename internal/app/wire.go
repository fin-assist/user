package app

import (
	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	tombstone "github.com/fin-assist/go-tombstone"

	consentgrpc "github.com/fin-assist/user/internal/adapter/grpc/consent"
	"github.com/fin-assist/user/internal/adapter/grpc/server"
	signinggrpc "github.com/fin-assist/user/internal/adapter/grpc/signing"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/consent"
	"github.com/fin-assist/user/internal/worker"
	"github.com/fin-assist/user/migrations"
)

// AccessPolicy lists only trusted callers and contract models for registered services.
func AccessPolicy() server.Policy {
	return server.Policy{
		userv1.ConsentDocumentService_GetConsentDocument_FullMethodName:     {Callers: []string{"bank-bff"}, Model: server.ModelNoUser},
		userv1.UserInternalService_GetSigningKeys_FullMethodName:            {Callers: []string{server.AnyCaller}, Model: server.ModelNoUser},
		userv1.UserInternalService_GetAssistantConsentStatus_FullMethodName: {Callers: []string{"assistant"}, Model: server.ModelSystem},
	}
}

// MigrationSources lists the migration sources in the order they are applied: libraries
// first (each with its own version table), then the service. A library exposes its files
// and table; add it here when the service starts using it, for example:
//
//	{Name: outbox.MigrationsName, Table: outbox.MigrationsTable, FS: outbox.Migrations},
func MigrationSources() []migrate.Source {
	return []migrate.Source{
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire registers the foundation RPCs. Auth and profile arrive in subsequent FIN-219 PRs.
func wire(a *App) (jobs []worker.Job, closeAll func(), err error) {
	documents := consent.New(postgres.NewConsentDocuments(a.DB))
	consentgrpc.Register(a.GRPC.GRPC(), documents, a.Users, a.Tombstones)
	signinggrpc.Register(a.GRPC.GRPC(), a.Signing)
	return nil, func() {}, nil
}
