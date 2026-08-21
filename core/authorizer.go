package core

import "context"

type AuthorizationAction string

const (
	AuthorizationPublish   AuthorizationAction = "publish"
	AuthorizationSubscribe AuthorizationAction = "subscribe"
)

type AuthorizationStage string

const (
	AuthorizationPreSession  AuthorizationStage = "pre_session"
	AuthorizationPostConnect AuthorizationStage = "post_connect"
)

// AuthorizationRequest is the protocol-independent input to the runtime
// authorization policy. Params contain credentials separate from StreamKey.
type AuthorizationRequest struct {
	Action     AuthorizationAction
	Stage      AuthorizationStage
	StreamKey  string
	Protocol   string
	RemoteAddr string
	Params     map[string]string
	Extra      map[string]any
}

// Authorizer owns publish and subscribe authorization decisions. Protocol
// modules only construct requests and enforce the returned result.
type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

type authorizerHolder struct {
	authorizer Authorizer
}

// SetAuthorizer replaces the process-wide media authorizer. A nil authorizer
// preserves the backward-compatible allow behavior.
func (s *Server) SetAuthorizer(authorizer Authorizer) {
	if authorizer == nil {
		s.authorizer.Store(nil)
		return
	}
	s.authorizer.Store(&authorizerHolder{authorizer: authorizer})
}

// Authorize applies the current process-wide authorizer.
func (s *Server) Authorize(ctx context.Context, request AuthorizationRequest) error {
	holder := s.authorizer.Load()
	if holder == nil {
		return nil
	}
	return holder.authorizer.Authorize(ctx, request)
}
