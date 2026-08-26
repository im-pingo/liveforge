package core

import "context"

// AuthorizationAction identifies the resource operation being admitted.
type AuthorizationAction string

const (
	AuthorizationPublish   AuthorizationAction = "publish"
	AuthorizationSubscribe AuthorizationAction = "subscribe"
)

// AuthorizationStage distinguishes admission before state allocation from a
// check performed after the transport has connected.
type AuthorizationStage string

const (
	AuthorizationPreSession  AuthorizationStage = "pre_session"
	AuthorizationPostConnect AuthorizationStage = "post_connect"
)

// AuthorizationRequest is the protocol-independent authorization input.
// Credentials belong in Params and are never mixed into StreamKey.
type AuthorizationRequest struct {
	Action     AuthorizationAction
	Stage      AuthorizationStage
	StreamKey  string
	Protocol   string
	RemoteAddr string
	Params     map[string]string
	Extra      map[string]any
}

// Authorizer owns the process-wide authorization decision. A nil authorizer
// preserves the event-bus hook behavior used by existing deployments.
type Authorizer interface {
	Authorize(context.Context, AuthorizationRequest) error
}

type authorizerHolder struct {
	authorizer Authorizer
}

// SetAuthorizer replaces the process-wide media authorizer. Passing nil
// restores the compatibility event-bus adapter.
func (s *Server) SetAuthorizer(authorizer Authorizer) {
	if authorizer == nil {
		s.authorizer.Store(nil)
		return
	}
	s.authorizer.Store(&authorizerHolder{authorizer: authorizer})
}

// Authorize applies the configured authorizer, or adapts the request to the
// existing synchronous event hook when no explicit authorizer is installed.
func (s *Server) Authorize(ctx context.Context, request AuthorizationRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if holder := s.authorizer.Load(); holder != nil && holder.authorizer != nil {
		return holder.authorizer.Authorize(ctx, request)
	}

	event := EventSubscribe
	if request.Action == AuthorizationPublish {
		event = EventPublish
	}
	return s.eventBus.EmitSync(event, &EventContext{
		StreamKey:  request.StreamKey,
		Protocol:   request.Protocol,
		RemoteAddr: request.RemoteAddr,
		Params:     cloneStringMap(request.Params),
		Extra:      cloneAnyMap(request.Extra),
	})
}

func cloneStringMap(src map[string]string) map[string]string {
	if src == nil {
		return nil
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func cloneAnyMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}
