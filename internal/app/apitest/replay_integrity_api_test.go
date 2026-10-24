package apitest_test

import (
	"testing"
	"time"

	userv1 "github.com/fin-assist/contracts/gen/go/finassist/user/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/fin-assist/user/internal/app/apitest"
)

func TestAPICachedStartRejectsCorruptedAttemptOwnershipWithoutRepeatingProvider(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"malformed-body-hash", `UPDATE phone_verification SET start_request_hash=decode('00','hex')`},
		{"changed-body-hash", `UPDATE phone_verification SET start_request_hash=decode(repeat('00',32),'hex')`},
		{"missing-window-set", `UPDATE phone_verification SET start_reserved_windows='null'::jsonb`},
		{"wrong-window-count", `UPDATE phone_verification SET start_reserved_windows='[{}]'::jsonb`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &creationProvider{}
			env := startEnv(t, p)
			key, phone := authKey(), "+79001234567"
			first, err := startCall(t, env, key, phone)
			if err != nil {
				t.Fatal(err)
			}
			var hash, windows []byte
			if err := env.App.DB.Primary().QueryRow(t.Context(), `SELECT start_request_hash,start_reserved_windows FROM phone_verification WHERE start_key=$1`, key).Scan(&hash, &windows); err != nil {
				t.Fatal(err)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.sql); err != nil {
				t.Fatal(err)
			}
			if got, err := startCall(t, env, key, phone); got != nil || err == nil {
				t.Fatal("corrupted ownership replay exposed a secret", got, err)
			}
			if creates, recovers := p.counts(); creates != 1 || recovers != 0 {
				t.Fatal("corruption repeated external effect", creates, recovers)
			}
			if hits := startHits(t, env); hits != 1 {
				t.Fatal("corrupt replay consumed another attempt", hits)
			}
			if _, err := env.App.DB.Primary().Exec(t.Context(), `UPDATE phone_verification SET start_request_hash=$1,start_reserved_windows=$2::jsonb`, hash, windows); err != nil {
				t.Fatal(err)
			}
			repaired, err := startCall(t, env, key, phone)
			if err != nil || repaired.GetVerificationToken() != first.GetVerificationToken() {
				t.Fatal(repaired, err)
			}
		})
	}
}

func TestAPIWatchRejectsInconsistentAuthenticatedRegistrationResults(t *testing.T) {
	for _, tc := range []struct{ name, sql string }{
		{"result-kind", `UPDATE phone_verification SET is_new_user=false`},
		{"missing-secret-hash", `UPDATE phone_verification SET registration_token_hash=NULL`},
		{"mismatched-secret-hash", `UPDATE phone_verification SET registration_token_hash=decode(repeat('00',32),'hex')`},
		{"malformed-secret-hash", `UPDATE phone_verification SET registration_token_hash=decode('00','hex')`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := apitest.Start(t)
			raw, phone := "integrity-public-watch", "+79001234567"
			pendingPhoneAPI(t, env, raw, phone, time.Now().Add(time.Minute))
			confirmPhoneAPI(t, env, raw, phone)
			if _, err := env.App.DB.Primary().Exec(t.Context(), tc.sql); err != nil {
				t.Fatal(err)
			}
			stream, err := userv1.NewAuthServiceClient(env.Conn).WatchPhoneVerification(phoneContext(t.Context()), &userv1.WatchPhoneVerificationRequest{VerificationToken: raw})
			if err != nil {
				t.Fatal(err)
			}
			got, err := stream.Recv()
			if got != nil || status.Code(err) != codes.Internal || apitest.Reason(err) != "" {
				t.Fatal(got, err)
			}
		})
	}
}
