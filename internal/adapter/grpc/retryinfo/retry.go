// Package retryinfo translates usecase-owned original-request advice to the contract.
package retryinfo

import (
	"errors"

	"github.com/fin-assist/contracts/gen/go/finerr"
)

// Apply preserves absence and attaches advice only to a valid mapped contract error.
func Apply(err error) error {
	var decision interface{ RequestRetryPermission() bool }
	if errors.As(err, &decision) {
		return finerr.WithRequestRetryInfo(err, decision.RequestRetryPermission())
	}
	return err
}
