package server

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/grpc"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Model is how a method is called (grpc-api.md, 3).
type Model int

const (
	// ModelUser: on behalf of a user; the access token in authorization is required, the
	// user is its sub (reqctx.UserID). Request messages carry no user_id.
	ModelUser Model = iota + 1
	// ModelSystem: a background call without a live user token; user_id is a request
	// field, the handler takes it from there. An authorization header is ignored.
	ModelSystem
	// ModelMixed: user call when authorization is present, system call otherwise
	// (GetAnalytics, ListPeriods, GetAssistantConsentStatus: optional user_id).
	ModelMixed
	// ModelNoUser: no user token yet (sign-in: AuthService). authorization is ignored.
	ModelNoUser
)

func (m Model) String() string {
	switch m {
	case ModelUser:
		return "user"
	case ModelSystem:
		return "system"
	case ModelMixed:
		return "mixed"
	case ModelNoUser:
		return "no-user"
	}
	return fmt.Sprintf("Model(%d)", int(m))
}

// AnyCaller in Rule.Callers allows every verified service (GetSigningKeys).
const AnyCaller = "*"

// Rule says who may call a method and how.
type Rule struct {
	Callers []string // ServiceAccount names (bank-bff, user, ...), or AnyCaller
	Model   Model
}

// Policy is the table «method → allowed callers → model» of grpc-api.md 3.3. A key is a
// full method ("/finassist.user.v1.ProfileService/GetProfile") or a whole service
// ("/finassist.user.v1.ProfileService/*"); an exact method wins over its service.
//
// A method that is not in the table is refused to everyone (fail closed), and Server.Serve
// refuses to start while a registered method has no rule.
type Policy map[string]Rule

func (p Policy) lookup(fullMethod string) (Rule, bool) {
	if r, ok := p[fullMethod]; ok {
		return r, true
	}
	if i := strings.LastIndex(fullMethod, "/"); i > 0 {
		r, ok := p[fullMethod[:i]+"/*"]
		return r, ok
	}
	return Rule{}, false
}

func (r Rule) allows(caller string) bool {
	return slices.Contains(r.Callers, AnyCaller) || slices.Contains(r.Callers, caller)
}

// open lists the services every caller reaches without a service token: health (probes,
// systemd checks) and reflection, which is registered only in dev.
var open = []string{healthpb.Health_ServiceDesc.ServiceName, "grpc.reflection.v1.ServerReflection", "grpc.reflection.v1alpha.ServerReflection"}

func isOpen(fullMethod string) bool {
	svc, _, _ := strings.Cut(strings.TrimPrefix(fullMethod, "/"), "/")
	return slices.Contains(open, svc)
}

// validate checks the table itself and that it covers every method of srv.
func (p Policy) validate(srv *grpc.Server) error {
	for key, r := range p {
		if !strings.HasPrefix(key, "/") || strings.Count(key, "/") != 2 {
			return fmt.Errorf("access policy: key %q is not /package.Service/Method or /package.Service/*", key)
		}
		if len(r.Callers) == 0 {
			return fmt.Errorf("access policy: %s has no callers", key)
		}
		if r.Model < ModelUser || r.Model > ModelNoUser {
			return fmt.Errorf("access policy: %s has no model", key)
		}
	}
	var missing []string
	for svc, info := range srv.GetServiceInfo() {
		for _, m := range info.Methods {
			full := "/" + svc + "/" + m.Name
			if isOpen(full) {
				continue
			}
			if _, ok := p.lookup(full); !ok {
				missing = append(missing, full)
			}
		}
	}
	if len(missing) > 0 {
		slices.Sort(missing)
		return fmt.Errorf("access policy has no rule for %s (grpc-api.md 3.3)", strings.Join(missing, ", "))
	}
	return nil
}
