package app

import (
	"errors"
	"time"

	goidentity "github.com/fin-assist/go-identity"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/go-grpc/client"
	"github.com/fin-assist/go-grpc/server"
	"github.com/fin-assist/go-worker"

	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/signing"
)

type identityRuntime struct {
	services server.ServiceVerifier
	users    server.UserVerifier
	tokens   client.TokenSource
	jobs     []worker.Job
	close    func()
}

// buildIdentity keeps the service's existing ACL/context chain; only its verifiers
// and outgoing token come from go-identity NewAssembly (FIN-325). Installing a
// second identity interceptor would lose reqctx. ENV selects the service identity:
// prod — Kubernetes ServiceAccount tokens, stage — JWTs signed with file keys.
// User access tokens are checked against this service's own signing keys through
// the local JWKS provider, never through a call to its own GetSigningKeys.
func buildIdentity(cfg config.Config, keys *signing.Keys, reg prometheus.Registerer) (identityRuntime, error) {
	r := identityRuntime{close: func() {}}
	if cfg.Auth.Mode == config.AuthInsecureDev {
		if cfg.Env != config.EnvDev {
			return r, errors.New("insecure-dev requires ENV=dev")
		}
		r.services, r.users, r.tokens = server.DevServiceVerifier{}, server.DevUserVerifier{}, client.DevToken(cfg.Service)
		return r, nil
	}
	if cfg.Auth.Mode != config.AuthVerify || keys == nil {
		return r, errors.New("identity: verify mode and signing keys required")
	}
	k, s := cfg.Auth.Kubernetes, cfg.Auth.Stage
	assembly, err := goidentity.NewAssembly(goidentity.AssemblyOptions{
		Env:     string(cfg.Env),
		Service: cfg.Service,
		// One source of truth: the issuer/audience this service signs is the one it accepts.
		UserIssuer:   cfg.Signing.Issuer,
		UserAudience: cfg.Signing.Audience,
		UserKeys:     keys,
		Kubernetes: goidentity.KubernetesOptions{
			TokenFile: cfg.Auth.ServiceTokenFile, Issuer: k.Issuer, JWKSURL: k.JWKSURL,
			CAFile: k.CAFile, APITokenFile: k.APITokenFile, Namespace: k.Namespace,
		},
		Stage:      goidentity.StageKeyFiles{Issuer: s.Issuer, SigningKeyFile: s.SigningKeyFile, PublicKeysFile: s.PublicKeysFile},
		Clock:      time.Now,
		Registerer: reg,
	})
	if err != nil {
		return r, err
	}
	r.services, r.users, r.tokens = assembly, assembly, assembly
	r.jobs = []worker.Job{{Name: "identity-keys", Run: assembly.Run}}
	r.close = func() { _ = assembly.Close() }
	return r, nil
}
