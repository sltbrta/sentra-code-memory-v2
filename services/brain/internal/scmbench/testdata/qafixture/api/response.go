package api

// JSON helpers for building response bodies. The fixture keeps these minimal
// but distinct so retrieval has real candidates to rank.

// OKResponse builds a 200 response with a JSON object body.
func OKResponse(payload string) *Response {
	return &Response{Status: 200, Body: []byte(`{"ok":true,"data":` + payload + `}`)}
}

// ErrorResponse builds a non-2xx response with a stable error code.
func ErrorResponse(status int, code, msg string) *Response {
	body := `{"ok":false,"code":"` + code + `","msg":"` + msg + `"}`
	return &Response{Status: status, Body: []byte(body)}
}

// NotFound is the canonical 404 response for unknown routes.
func NotFound() *Response {
	return ErrorResponse(404, "not_found", "route not found")
}

// Unauthorized is the canonical 401 response for failed token validation.
func Unauthorized() *Response {
	return ErrorResponse(401, "unauthorized", "token validation failed")
}
