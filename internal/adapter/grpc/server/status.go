package server

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// bare is the only place outside finerr that builds a status: refusals between services
// that never reach the client carry a bare code without ErrorInfo (grpc-api.md 5.1,
// rule 3); bank-bff turns them into 500. The message is generic, so it reveals nothing to
// a foreign caller; the cause goes to our log.
func bare(c codes.Code) error {
	return status.Error(c, c.String()) //nolint:forbidigo // bare internal code, grpc-api.md 5.1 rule 3
}

// normalize returns what the caller may see of err. A status built by finerr or bare passes
// as is. A status wrapped by fmt.Errorf is unwrapped: grpc-go would otherwise send the
// wrapping text as status.message. Any other error becomes a bare code: grpc-go would send
// it as UNKNOWN with err.Error() as the message, and that text (a pgx error with a table
// name, a wrapped DSN) would leave the process. Context errors keep their codes so the
// caller can tell a timeout from a defect. The original error goes to the log line.
func normalize(err error) error {
	if err == nil {
		return nil
	}
	var gs interface{ GRPCStatus() *status.Status }
	if errors.As(err, &gs) {
		st := gs.GRPCStatus()
		if st == nil || st.Code() == codes.OK {
			// A status of OK as an error would make st.Err() nil and the failed call a
			// success with a nil response; neither finerr nor bare builds one.
			return bare(codes.Internal)
		}
		return st.Err()
	}
	switch {
	case errors.Is(err, context.Canceled):
		return bare(codes.Canceled)
	case errors.Is(err, context.DeadlineExceeded):
		return bare(codes.DeadlineExceeded)
	}
	return bare(codes.Internal)
}

// Reason returns ErrorInfo.reason of err: "" for nil, a bare status or a non-status error.
func Reason(err error) string {
	var se interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &se) {
		return ""
	}
	for _, d := range se.GRPCStatus().Details() {
		m, ok := d.(proto.Message)
		if !ok || m.ProtoReflect().Descriptor().FullName() != "google.rpc.ErrorInfo" {
			continue
		}
		if r, ok := d.(interface{ GetReason() string }); ok {
			return r.GetReason()
		}
	}
	return ""
}
