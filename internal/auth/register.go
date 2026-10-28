package auth

import (
	"context"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/fin-assist/contracts/gen/go/finerr"
	"google.golang.org/genproto/googleapis/rpc/errdetails"

	"github.com/fin-assist/user/internal/domain"
	"github.com/fin-assist/user/internal/token"
)

// Register consumes a confirmed new-user result atomically with account, consent,
// session and initial timezone. If another confirmed request created the phone's
// account first, it issues a session without changing that account's settings or
// consent history (owner's decision, FIN-219).
func (s *Service) Register(ctx context.Context, in RegisterInput) (out Registered, err error) {
	err = s.tx.Run(ctx, func(ctx context.Context) error {
		hash := token.Sum(in.Token)
		v, err := s.repo.Registration(ctx, hash, false)
		if err != nil {
			return missingToken(err)
		}
		now := s.now().UTC()
		if err := usableRegistration(v, now); err != nil {
			return err
		}
		zone := "Europe/Moscow"
		if in.Timezone != nil {
			zone = *in.Timezone
		}
		if !domain.ValidTimezone(zone) {
			return domain.WithRequestRetry(finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "timezone", Reason: "INVALID_FORMAT"}), false)
		}
		n := utf8.RuneCountInString(in.ConsentVersion)
		if n < 1 || n > 64 || !utf8.ValidString(in.ConsentVersion) {
			return domain.WithRequestRetry(finerr.ValidationError(&errdetails.BadRequest_FieldViolation{Field: "pd_consent_version", Reason: "INVALID_FORMAT"}), false)
		}
		if !in.Accepted {
			return domain.WithRequestRetry(finerr.ConsentRequired(), false)
		}
		if in.ConsentVersion != v.ConsentVersion {
			return domain.WithRequestRetry(finerr.ConsentOutdated(), false)
		}
		version := v.ConsentVersion
		id, created, err := s.repo.CreateUser(ctx, v.Phone, zone, now)
		if err != nil {
			return err
		}
		alive, err := s.guard.Guard(ctx, id)
		if err != nil {
			return err
		}
		if !alive {
			return domain.WithRequestRetry(finerr.InvalidToken(), false)
		}
		if err := s.repo.LockUser(ctx, id); err != nil {
			return missingToken(err)
		}
		// Guard -> user -> verification is also the deletion lock order. The
		// speculative INSERT above rolls back if the token was consumed meanwhile.
		v, err = s.repo.Registration(ctx, hash, true)
		if err != nil {
			return missingToken(err)
		}
		now = s.now().UTC()
		if err := usableRegistration(v, now); err != nil {
			return err
		}
		if now.Before(*v.VerifiedAt) {
			return errors.New("auth: future verification timestamp")
		}
		if created {
			if err := s.repo.GrantPD(ctx, id, version, v.ConsentAcceptedAt); err != nil {
				return err
			}
			if err := s.events.TimezoneChanged(ctx, id, zone, 1); err != nil {
				return err
			}
		}
		pair, err := s.pair(ctx, id)
		if err != nil {
			return err
		}
		if err := s.repo.CreateSession(ctx, id, v.ID, token.Sum(pair.Refresh), now, now.Add(s.refreshTTL)); err != nil {
			return missingToken(err)
		}
		if err := s.repo.CloseVerification(ctx, v.ID, id, now); err != nil {
			return missingToken(err)
		}
		out = Registered{UserID: id, Pair: pair}
		return nil
	})
	return out, err
}

func usableRegistration(v Registration, now time.Time) error {
	if v.VerifiedAt == nil || !v.IsNewUser || v.Closed || v.ConsentVersion == "" || v.ConsentAcceptedAt.IsZero() || v.AgeConfirmedAt.IsZero() {
		return domain.WithRequestRetry(finerr.InvalidToken(), false)
	}
	if !now.Before(v.VerifiedAt.Add(15 * time.Minute)) {
		return domain.WithRequestRetry(finerr.TokenExpired(), false)
	}
	return nil
}
