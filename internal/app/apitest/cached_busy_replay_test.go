package apitest_test

import (
	"context"
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"github.com/fin-assist/contracts/gen/go/finerr"
	idempotency "github.com/fin-assist/go-idempotency"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/fin-assist/user/internal/config"
)

// Seed through the public producer API, then replay through the assembled user
// service. No payload SQL or service permission override participates in the test.
func TestAPICachedBusyReplayPreservesStructuredDetailsAndAbsoluteExpiry(t *testing.T) {
	for _, explicitFalse := range []bool{false, true} {
		name := "absent"
		if explicitFalse {
			name = "explicit-false"
		}
		t.Run(name, func(t *testing.T) {
			provider := &creationProvider{}
			env := startEnv(t, provider)
			secret, err := config.LoadSessionKey(env.App.Cfg.Sessions)
			if err != nil {
				t.Fatal(err)
			}
			cipher, err := idempotency.NewCipher(secret)
			if err != nil {
				t.Fatal(err)
			}
			anchor := time.Now().UTC().Truncate(time.Second)
			seedAt, retryAt := anchor.Add(-2*time.Minute), anchor.Add(2*time.Minute)
			method := userv1.AuthService_StartPhoneVerification_FullMethodName
			engine, err := idempotency.New(env.App.DB, idempotency.Options{Clock: func() time.Time { return seedAt }, Cipher: cipher, Registerer: prometheus.NewRegistry(), Methods: map[string]idempotency.Policy{method: {Encrypt: true}}})
			if err != nil {
				t.Fatal(err)
			}
			in := &userv1.StartPhoneVerificationRequest{Phone: "+79001234567"}
			hash, err := idempotency.HashProto(in)
			if err != nil {
				t.Fatal(err)
			}
			key := authKey()
			request := idempotency.Request{Endpoint: method, Key: key, Hash: hash}
			wire := status.Convert(finerr.RateLimited(retryAt)).Proto()
			// The saved reply was produced two minutes ago. Its original relative wait
			// cannot be advertised unchanged by the current RPC consumer.
			originalWait, err := anypb.New(&errdetails.RetryInfo{RetryDelay: durationpb.New(retryAt.Sub(seedAt))})
			if err != nil {
				t.Fatal(err)
			}
			for i, detail := range wire.Details {
				if detail.MessageIs(&errdetails.RetryInfo{}) {
					wire.Details[i] = originalWait
				}
			}
			extension := &anypb.Any{TypeUrl: "type.googleapis.com/finassist.test.FutureRetryContext", Value: []byte{8, 1}}
			wire.Details = append(wire.Details, extension)
			refusal := status.FromProto(wire).Err()
			if explicitFalse {
				refusal = finerr.WithRequestRetryInfo(refusal, false)
			}
			completion, cancel := engine.WithCompletion(t.Context())
			defer cancel()
			if err := env.App.DB.Run(completion, func(tx context.Context) error {
				result, err := engine.Do(tx, request, func(context.Context) (idempotency.Response, error) {
					return idempotency.Response{DuplicateRetryAt: retryAt}, refusal
				})
				if err == nil && (result.Replayed || status.Code(result.Err) != codes.ResourceExhausted) {
					t.Error("producer did not persist the fresh refusal")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				_, err := startCall(t, env, key, in.Phone)
				rest, ok := finerr.ToREST(err)
				if !ok || status.Code(err) != codes.ResourceExhausted || rest.Code != "RATE_LIMITED" || rest.Metadata["retry_at"] != retryAt.Format(time.RFC3339) {
					t.Fatal("busy error lost its original binding", err)
				}
				if explicitFalse {
					assertRequestPermission(t, err, false)
				} else if rest.RequestRetryable != nil {
					t.Fatal("bare reason acquired request permission")
				}
				foundExtension, foundWait := false, false
				for _, detail := range status.Convert(err).Proto().Details {
					if detail.TypeUrl == extension.TypeUrl {
						foundExtension = proto.Equal(detail, extension)
					}
					if detail.MessageIs(&errdetails.RetryInfo{}) {
						wait := new(errdetails.RetryInfo)
						if err := detail.UnmarshalTo(wait); err != nil {
							t.Fatal(err)
						}
						duration := wait.GetRetryDelay().AsDuration()
						if duration <= 0 || duration > 2*time.Minute || duration > time.Until(retryAt)+time.Second {
							t.Fatal("replay restarted old relative wait", duration)
						}
						foundWait = true
					}
				}
				if !foundExtension || !foundWait {
					t.Fatal("cached structured details disappeared")
				}
				var expires time.Time
				var encrypted, done bool
				if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT expires_at,encrypted,done FROM idempotency_keys WHERE key=$1 AND endpoint=$2`, key, method).Scan(&expires, &encrypted, &done); err != nil {
					t.Fatal(err)
				}
				if !expires.Equal(retryAt) || !encrypted || !done {
					t.Fatal("replay changed expiry or encrypted completion")
				}
			}
			creates, recovers := provider.counts()
			if creates != 0 || recovers != 0 {
				t.Fatal("cached refusal repeated provider effect", creates, recovers)
			}
		})
	}
}
