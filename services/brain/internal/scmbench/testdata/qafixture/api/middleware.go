package api

// Middleware wraps an HTTP handler with cross-cutting behavior. The fixture
// ships authentication, logging, and recovery middleware to give retrieval a
// realistic set of similarly-named candidates.
type Middleware func(Handler) Handler

// Handler is the minimal HTTP handler shape used by the fixture router.
type Handler func(req *Request) *Response

// Request is the inbound HTTP request envelope.
type Request struct {
	Method string
	Path   string
	Token  string
	Body   []byte
}

// Response is the outbound HTTP response envelope.
type Response struct {
	Status int
	Body   []byte
}

// AuthMiddleware validates the bearer token before the wrapped handler runs.
// Requests without a valid token receive a 401 unauthorized response.
func AuthMiddleware(validate func(string) (string, error)) Middleware {
	return func(next Handler) Handler {
		return func(req *Request) *Response {
			if _, err := validate(req.Token); err != nil {
				return &Response{Status: 401, Body: []byte("unauthorized")}
			}
			return next(req)
		}
	}
}

// LoggingMiddleware records the method and path of every request.
func LoggingMiddleware(log func(string)) Middleware {
	return func(next Handler) Handler {
		return func(req *Request) *Response {
			log(req.Method + " " + req.Path)
			return next(req)
		}
	}
}

// RecoveryMiddleware converts panics in the wrapped handler into 500 responses
// so a single bad request does not crash the server.
func RecoveryMiddleware() Middleware {
	return func(next Handler) Handler {
		return func(req *Request) (resp *Response) {
			defer func() {
				if r := recover(); r != nil {
					resp = &Response{Status: 500, Body: []byte("internal error")}
				}
			}()
			return next(req)
		}
	}
}

// Chain applies middleware in order so the first listed runs outermost.
func Chain(h Handler, mws ...Middleware) Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}
