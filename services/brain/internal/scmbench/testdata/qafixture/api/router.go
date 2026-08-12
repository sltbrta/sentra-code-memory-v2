package api

// Router dispatches inbound requests to registered handlers by method and
// path. It is intentionally small but exposes enough surface to matter for
// retrieval ranking.
type Router struct {
	routes map[string]Handler
}

// NewRouter builds an empty router.
func NewRouter() *Router {
	return &Router{routes: map[string]Handler{}}
}

// Route registers a handler for a method and path pair.
func (r *Router) Route(method, path string, h Handler) {
	r.routes[method+" "+path] = h
}

// Dispatch finds and invokes the handler matching the request. Unknown routes
// receive a 404 not-found response.
func (r *Router) Dispatch(req *Request) *Response {
	h, ok := r.routes[req.Method+" "+req.Path]
	if !ok {
		return &Response{Status: 404, Body: []byte("not found")}
	}
	return h(req)
}

// DefaultRouter wires the authentication and query endpoints with the standard
// middleware chain used by the fixture server.
func DefaultRouter(validate func(string) (string, error)) *Router {
	r := NewRouter()
	auth := Chain(func(req *Request) *Response {
		return &Response{Status: 200, Body: []byte(HandleAuth(req.Token))}
	}, AuthMiddleware(validate), RecoveryMiddleware())
	query := Chain(func(req *Request) *Response {
		return &Response{Status: 200, Body: []byte(HandleQuery(req.Token, string(req.Body)))}
	}, AuthMiddleware(validate), RecoveryMiddleware())
	r.Route("POST", "/auth", auth)
	r.Route("POST", "/query", query)
	return r
}
