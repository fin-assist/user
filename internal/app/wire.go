package app

import (
	"context"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	outboxlib "github.com/fin-assist/go-outbox"
	tombstone "github.com/fin-assist/go-tombstone"
	"github.com/prometheus/client_golang/prometheus"

	consentgrpc "github.com/fin-assist/user/internal/adapter/grpc/consent"
	profilegrpc "github.com/fin-assist/user/internal/adapter/grpc/profile"
	"github.com/fin-assist/user/internal/adapter/grpc/server"
	signinggrpc "github.com/fin-assist/user/internal/adapter/grpc/signing"
	"github.com/fin-assist/user/internal/adapter/postgres"
	"github.com/fin-assist/user/internal/adapter/postgres/migrate"
	"github.com/fin-assist/user/internal/consent"
	"github.com/fin-assist/user/internal/profile"
	"github.com/fin-assist/user/internal/worker"
	"github.com/fin-assist/user/migrations"
)

// AccessPolicy lists only trusted callers and contract models for registered services.
func AccessPolicy() server.Policy {
	return server.Policy{
		"/finassist.user.v1.ProfileService/*":                               {Callers: []string{"bank-bff"}, Model: server.ModelUser},
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
		{Name: outboxlib.MigrationsName, Table: outboxlib.MigrationsTable, FS: outboxlib.Migrations},
		{Name: tombstone.MigrationsName, Table: tombstone.MigrationsTable, FS: tombstone.Migrations},
		{Name: "service", Table: "schema_migrations", FS: migrations.FS},
	}
}

// wire registers document, signing, profile and consent RPCs plus durable timezone delivery.
func wire(ctx context.Context, a *App, reg prometheus.Registerer) (jobs []worker.Job, closeAll func(), err error) {
	events, dispatch, closeOutbox, err := wireOutbox(ctx, a, reg)
	if err != nil {
		return nil, nil, err
	}
	profiles := profile.New(a.DB, postgres.NewProfiles(a.DB), a.Tombstones, events, time.Now)
	profilegrpc.Register(a.GRPC.GRPC(), profiles)
	documents := consent.New(postgres.NewConsentDocuments(a.DB))
	consentgrpc.Register(a.GRPC.GRPC(), documents, a.Users, a.Tombstones)
	signinggrpc.Register(a.GRPC.GRPC(), a.Signing, profiles)
	limits := postgres.NewLimits(a.DB)
	cleanup := worker.Every("rate-limit-cleanup", time.Minute, a.Log, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := limits.Cleanup(ctx, time.Now().Add(-time.Hour), 1000)
		return err
	})
	return []worker.Job{cleanup, dispatch}, closeOutbox, nil
}
