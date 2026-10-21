package service

import (
	"fmt"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/Tencent/WeKnora/internal/types"
)

// invocationContext is the validated platform-signed call context (BND-150).
type invocationContext struct {
	ActorKind string
	TaskID    string
	Device    string
	JTI       string
}

// invocationContextClaims is the claim set the platform signs for one
// invocation. The context binds the actor kind, the unattended task binding
// and the canonical digest of the invocation arguments; the user and tenant
// claims must match the authenticated caller.
type invocationContextClaims struct {
	ActorKind string `json:"actor_kind"`
	TaskID    string `json:"task_id"`
	Device    string `json:"device"`
	Tenant    uint64 `json:"tenant"`
	// ArgsDigest is the platform-computed MCPInvocationArgsDigest of the exact
	// arguments this context authorizes. A call whose arguments digest
	// differently is rejected, so a context cannot be reshaped onto a
	// different call.
	ArgsDigest string `json:"args_digest"`
	jwt.RegisteredClaims
}

// validateContext parses and checks the platform-signed invocation context.
// The context is mandatory (BND-150): an empty raw context is rejected with
// ErrMCPInvocationContextInvalid (HTTP 403 CONTEXT_INVALID), as is anything
// that fails signature, freshness, audience, subject, tenant, digest or
// replay validation.
func (s *MCPInvocationService) validateContext(
	caller MCPInvocationCaller, raw, argsDigest string,
) (*invocationContext, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("%w: an invocation context is required", ErrMCPInvocationContextInvalid)
	}
	key := ""
	if s.cfg != nil {
		key = strings.TrimSpace(s.cfg.ContextKey)
	}
	if key == "" {
		return nil, fmt.Errorf("%w: no platform context key is configured", ErrMCPInvocationContextInvalid)
	}

	claims := &invocationContextClaims{}
	token, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(key), nil
	})
	if err != nil || token == nil || !token.Valid {
		return nil, fmt.Errorf("%w: %v", ErrMCPInvocationContextInvalid, err)
	}

	now := s.now()
	maxAge := s.cfg.ContextMaxAge()
	switch {
	case claims.ExpiresAt == nil:
		return nil, fmt.Errorf("%w: exp is required", ErrMCPInvocationContextInvalid)
	case !claims.ExpiresAt.Time.After(now):
		return nil, fmt.Errorf("%w: context has expired", ErrMCPInvocationContextInvalid)
	case claims.ExpiresAt.Time.Sub(now) > maxAge:
		return nil, fmt.Errorf("%w: context lifetime exceeds %s", ErrMCPInvocationContextInvalid, maxAge)
	}
	// Fail closed on audience: without a configured audience no signed
	// context is acceptable, whatever it claims.
	audience := s.cfg.Audience()
	if audience == "" {
		return nil, fmt.Errorf("%w: no invocation context audience is configured", ErrMCPInvocationContextInvalid)
	}
	matched := false
	for _, aud := range claims.Audience {
		if strings.TrimSpace(aud) == audience {
			matched = true
			break
		}
	}
	if !matched {
		return nil, fmt.Errorf("%w: audience mismatch", ErrMCPInvocationContextInvalid)
	}
	if strings.TrimSpace(claims.Subject) != strings.TrimSpace(caller.UserID) {
		return nil, fmt.Errorf("%w: context user does not match the authenticated subject", ErrMCPInvocationContextInvalid)
	}
	if claims.Tenant == 0 || claims.Tenant != caller.TenantID {
		return nil, fmt.Errorf("%w: context tenant does not match the authenticated tenant", ErrMCPInvocationContextInvalid)
	}
	// The context must bind the exact arguments of this call: a context minted
	// for different arguments cannot authorize them.
	if strings.TrimSpace(claims.ArgsDigest) != strings.TrimSpace(argsDigest) {
		return nil, fmt.Errorf("%w: context args_digest does not match the invocation arguments", ErrMCPInvocationContextInvalid)
	}
	jti := strings.TrimSpace(claims.ID)
	if jti == "" {
		return nil, fmt.Errorf("%w: jti is required", ErrMCPInvocationContextInvalid)
	}
	if s.jtiSeen(jti) {
		return nil, fmt.Errorf("%w: context replay detected", ErrMCPInvocationContextInvalid)
	}

	actorKind := strings.TrimSpace(claims.ActorKind)
	if actorKind == "" {
		actorKind = types.MCPInvocationActorUser
	}
	if actorKind != types.MCPInvocationActorUser && actorKind != types.MCPInvocationActorTask {
		return nil, fmt.Errorf("%w: unknown actor_kind %q", ErrMCPInvocationContextInvalid, claims.ActorKind)
	}
	out := &invocationContext{ActorKind: actorKind, JTI: jti}
	if actorKind == types.MCPInvocationActorTask {
		out.TaskID = strings.TrimSpace(claims.TaskID)
		out.Device = strings.TrimSpace(claims.Device)
		if out.TaskID == "" || out.Device == "" {
			return nil, fmt.Errorf("%w: task contexts must bind task_id and device", ErrMCPInvocationContextInvalid)
		}
	}
	return out, nil
}
