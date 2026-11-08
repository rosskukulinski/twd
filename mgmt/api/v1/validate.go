// Package v1 is the generated /v1 contract (types, server interfaces, the
// embedded spec) plus this small hand-written validation helper. Everything
// in api.gen.go comes from api/openapi.yaml via `make api`.
package v1

import (
	"context"
	"fmt"
	"net/http"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
)

// Spec returns the embedded, parsed OpenAPI document. The servers list is
// cleared so that request validation matches on path alone (tests dial
// arbitrary hosts and unix sockets).
func Spec() (*openapi3.T, error) {
	doc, err := GetSwagger()
	if err != nil {
		return nil, err
	}
	doc.Servers = nil
	return doc, nil
}

// Validator validates HTTP requests and responses against the spec. Tests
// wrap mgmt's handler with Middleware so every exchange in the hermetic
// suites is checked; production handlers do not pay for it.
type Validator struct {
	doc    *openapi3.T
	router routers.Router
}

// NewValidator builds a Validator from the embedded spec.
func NewValidator() (*Validator, error) {
	doc, err := Spec()
	if err != nil {
		return nil, err
	}
	if err := doc.Validate(context.Background(), openapi3.DisableExamplesValidation()); err != nil {
		return nil, fmt.Errorf("openapi: %w", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		return nil, err
	}
	return &Validator{doc: doc, router: router}, nil
}

// authOK accepts every security scheme: authentication is WhoIs on the
// connection (or the local-dev token), checked by mgmt itself, not by the
// validator.
func authOK(context.Context, *openapi3filter.AuthenticationInput) error { return nil }

// ValidateRequest checks a request against the spec and returns the matched
// route (nil when the path is not in the spec).
func (v *Validator) ValidateRequest(r *http.Request) (*routers.Route, error) {
	route, pathParams, err := v.router.FindRoute(r)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", r.Method, r.URL.Path, err)
	}
	in := &openapi3filter.RequestValidationInput{
		Request:    r,
		PathParams: pathParams,
		Route:      route,
		Options:    &openapi3filter.Options{AuthenticationFunc: authOK, MultiError: true},
	}
	if err := openapi3filter.ValidateRequest(r.Context(), in); err != nil {
		return route, fmt.Errorf("%s %s: request: %w", r.Method, r.URL.Path, err)
	}
	return route, nil
}

// ValidateResponse checks a response (status, headers, JSON body) against
// the route matched for its request.
func (v *Validator) ValidateResponse(r *http.Request, route *routers.Route, pathParams map[string]string, status int, header http.Header, body []byte) error {
	in := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: &openapi3filter.RequestValidationInput{
			Request:    r,
			PathParams: pathParams,
			Route:      route,
			Options:    &openapi3filter.Options{AuthenticationFunc: authOK, IncludeResponseStatus: true},
		},
		Status: status,
		Header: header,
		Options: &openapi3filter.Options{
			IncludeResponseStatus: true,
			MultiError:            true,
		},
	}
	in.SetBodyBytes(body)
	if err := openapi3filter.ValidateResponse(r.Context(), in); err != nil {
		return fmt.Errorf("%s %s: response %d: %w", r.Method, r.URL.Path, status, err)
	}
	return nil
}

// Middleware validates every request and response that passes through h.
// A request that fails validation is answered 400 with a typed Error
// (code bad_request); a response that fails is reported through report
// (the test's t.Errorf) after it has been sent unchanged.
func (v *Validator) Middleware(report func(error)) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route, pathParams, err := v.router.FindRoute(r)
			if err != nil {
				report(fmt.Errorf("%s %s: not in the spec: %w", r.Method, r.URL.Path, err))
				h.ServeHTTP(w, r)
				return
			}
			if _, err := v.ValidateRequest(r); err != nil {
				report(err)
			}
			rec := &recorder{ResponseWriter: w, status: 200}
			h.ServeHTTP(rec, r)
			if err := v.ValidateResponse(r, route, pathParams, rec.status, rec.Header(), rec.body); err != nil {
				report(err)
			}
		})
	}
}

type recorder struct {
	http.ResponseWriter
	status int
	body   []byte
	wrote  bool
}

func (r *recorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	r.wrote = true
	r.body = append(r.body, p...)
	return r.ResponseWriter.Write(p)
}
