// Package domain contains value rules shared by the service's usecases.
package domain

import (
	"time"
	_ "time/tzdata" // The distroless image must validate the same zones as local Go.
)

// ValidTimezone accepts a named IANA zone within the contract's length bound.
func ValidTimezone(s string) bool {
	if s == "" || s == "Local" || len(s) > 64 {
		return false
	}
	_, err := time.LoadLocation(s)
	return err == nil
}
