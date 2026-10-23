package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"time"

	goidentity "github.com/fin-assist/go-identity"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fin-assist/user/internal/adapter/client"
	"github.com/fin-assist/user/internal/adapter/grpc/server"
	"github.com/fin-assist/user/internal/config"
	"github.com/fin-assist/user/internal/signing"
	"github.com/fin-assist/user/internal/worker"
)

type identityRuntime struct {
	services server.ServiceVerifier
	users    server.UserVerifier
	tokens   client.TokenSource
	jobs     []worker.Job
	close    func()
}

// Keep the service's existing ACL/context chain; only its cryptographic verifiers
// come from go-identity. Installing a second identity interceptor loses reqctx.
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
	metrics, err := goidentity.NewMetrics(reg)
	if err != nil {
		return r, err
	}
	cache, err := goidentity.NewKeyCache(keys, time.Now, 10*time.Minute)
	if err != nil {
		return r, err
	}
	r.users, err = goidentity.NewUserVerifier(cache, goidentity.VerifyOptions{Issuer: cfg.Signing.Issuer, Audience: cfg.Signing.Audience, Clock: time.Now, Observer: metrics})
	if err != nil {
		return r, err
	}
	r.jobs = append(r.jobs, worker.Job{Name: "identity-user-keys", Run: cache.Run})
	opts := goidentity.VerifyOptions{Issuer: cfg.Auth.ServiceIssuer, Audience: "finassist-internal", Clock: time.Now, Observer: metrics}
	switch cfg.Auth.ServiceMode {
	case "stage":
		public, err := signing.StagePublicKeys(cfg.Auth.StagePublicKeysFile)
		if err != nil {
			return r, err
		}
		r.services, err = goidentity.NewStageVerifier(string(cfg.Env), opts, public)
		if err != nil {
			return r, err
		}
		r.tokens, err = goidentity.NewStageSource(string(cfg.Env), goidentity.StageOptions{KeyFile: cfg.Auth.StageKeyFile, Caller: cfg.Service, Issuer: cfg.Auth.ServiceIssuer, Clock: time.Now})
		return r, err
	case "kubernetes":
		tokens, err := goidentity.NewProjectedSource(cfg.Auth.ServiceTokenFile)
		if err != nil {
			return r, err
		}
		apiToken, err := goidentity.NewProjectedSource(cfg.Auth.KubernetesTokenFile)
		if err != nil {
			return r, err
		}
		ca, err := os.ReadFile(cfg.Auth.KubernetesCAFile)
		if err != nil {
			return r, errors.New("identity: Kubernetes CA unavailable")
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(ca) {
			return r, errors.New("identity: invalid Kubernetes CA")
		}
		transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second}
		r.close = transport.CloseIdleConnections
		provider, err := goidentity.NewHTTPKeys(&http.Client{Transport: transport, Timeout: 5 * time.Second}, cfg.Auth.KubernetesJWKSURL, apiToken)
		if err != nil {
			r.close()
			return r, err
		}
		cache, err := goidentity.NewKeyCache(provider, time.Now, time.Hour)
		if err != nil {
			r.close()
			return r, err
		}
		r.services, err = goidentity.NewKubernetesVerifier(cache, opts, "finassist")
		if err != nil {
			r.close()
			return r, err
		}
		r.tokens = tokens
		r.jobs = append(r.jobs, worker.Job{Name: "identity-kubernetes-keys", Run: cache.Run})
		return r, nil
	default:
		return r, errors.New("identity: unsupported service auth mode")
	}
}
