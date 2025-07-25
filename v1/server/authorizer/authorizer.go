// Copyright 2017 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// Package authorizer provides authorization handlers to the server.
package authorizer

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/open-policy-agent/opa/v1/ast"
	"github.com/open-policy-agent/opa/v1/rego"
	"github.com/open-policy-agent/opa/v1/server/identifier"
	"github.com/open-policy-agent/opa/v1/server/types"
	"github.com/open-policy-agent/opa/v1/server/writer"
	"github.com/open-policy-agent/opa/v1/storage"
	"github.com/open-policy-agent/opa/v1/topdown/cache"
	"github.com/open-policy-agent/opa/v1/topdown/print"
	"github.com/open-policy-agent/opa/v1/util"
)

// Basic provides policy-based authorization over incoming requests.
type Basic struct {
	inner                  http.Handler
	compiler               func() *ast.Compiler
	store                  storage.Store
	runtime                *ast.Term
	decision               func() ast.Ref
	printHook              print.Hook
	enablePrintStatements  bool
	interQueryCache        cache.InterQueryCache
	interQueryValueCache   cache.InterQueryValueCache
	urlPathExpectsBodyFunc []func(string, []any) bool
}

// Runtime returns an argument that sets the runtime on the authorizer.
func Runtime(term *ast.Term) func(*Basic) {
	return func(b *Basic) {
		b.runtime = term
	}
}

// Decision returns an argument that sets the path of the authorization decision
// to query.
func Decision(ref func() ast.Ref) func(*Basic) {
	return func(b *Basic) {
		b.decision = ref
	}
}

// PrintHook sets the object to use for handling print statement outputs.
func PrintHook(printHook print.Hook) func(*Basic) {
	return func(b *Basic) {
		b.printHook = printHook
	}
}

// EnablePrintStatements enables print() calls. If this option is not provided,
// print() calls will be erased from the policy. This option only applies to
// queries and policies that passed as raw strings, i.e., this function will not
// have any affect if the caller supplies the ast.Compiler instance.
func EnablePrintStatements(yes bool) func(r *Basic) {
	return func(b *Basic) {
		b.enablePrintStatements = yes
	}
}

// InterQueryCache enables the inter-query cache on the authorizer
func InterQueryCache(interQueryCache cache.InterQueryCache) func(*Basic) {
	return func(b *Basic) {
		b.interQueryCache = interQueryCache
	}
}

// InterQueryValueCache enables the inter-query value cache on the authorizer
func InterQueryValueCache(interQueryValueCache cache.InterQueryValueCache) func(*Basic) {
	return func(b *Basic) {
		b.interQueryValueCache = interQueryValueCache
	}
}

// URLPathValidatorFuncs allows for extensions to the allowed paths an authorizer will accept.
func URLPathExpectsBodyFunc(urlPathExpectsBodyFunc []func(string, []any) bool) func(*Basic) {
	return func(b *Basic) {
		b.urlPathExpectsBodyFunc = urlPathExpectsBodyFunc
	}
}

// NewBasic returns a new Basic object.
func NewBasic(inner http.Handler, compiler func() *ast.Compiler, store storage.Store, opts ...func(*Basic)) http.Handler {
	b := &Basic{
		inner:    inner,
		compiler: compiler,
		store:    store,
	}

	for _, opt := range opts {
		opt(b)
	}

	return b
}

func (b *Basic) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// TODO(tsandall): Pass AST value as input instead of Go value to avoid unnecessary
	// conversions.
	r, input, err := makeInput(r, b.urlPathExpectsBodyFunc)
	if err != nil {
		writer.ErrorString(w, http.StatusBadRequest, types.CodeInvalidParameter, err)
		return
	}

	rego := rego.New(
		rego.Query(b.decision().String()),
		rego.Compiler(b.compiler()),
		rego.Store(b.store),
		rego.Input(input),
		rego.Runtime(b.runtime),
		rego.EnablePrintStatements(b.enablePrintStatements),
		rego.PrintHook(b.printHook),
		rego.InterQueryBuiltinCache(b.interQueryCache),
		rego.InterQueryBuiltinValueCache(b.interQueryValueCache),
	)

	rs, err := rego.Eval(r.Context())
	if err != nil {
		writer.ErrorAuto(w, err)
		return
	}

	if len(rs) == 0 {
		// Authorizer was configured but no policy defined. This indicates an internal error or misconfiguration.
		writer.Error(w, http.StatusInternalServerError, types.NewErrorV1(types.CodeInternal, types.MsgUnauthorizedUndefinedError))
		return
	}

	switch allowed := rs[0].Expressions[0].Value.(type) {
	case bool:
		if allowed {
			b.inner.ServeHTTP(w, r)
			return
		}
	case map[string]any:
		if decision, ok := allowed["allowed"]; ok {
			if allow, ok := decision.(bool); ok && allow {
				b.inner.ServeHTTP(w, r)
				return
			}
			if reason, ok := allowed["reason"]; ok {
				message, ok := reason.(string)
				if ok {
					writer.Error(w, http.StatusUnauthorized, types.NewErrorV1(types.CodeUnauthorized, message)) //nolint:govet
					return
				}
			}
		} else {
			writer.Error(w, http.StatusInternalServerError, types.NewErrorV1(types.CodeInternal, types.MsgUndefinedError))
			return
		}
	}
	writer.Error(w, http.StatusUnauthorized, types.NewErrorV1(types.CodeUnauthorized, types.MsgUnauthorizedError))
}

var emptyQuery = url.Values{}

func makeInput(r *http.Request, extraPaths []func(string, []any) bool) (*http.Request, any, error) {
	path, err := parsePath(r.URL.Path)
	if err != nil {
		return r, nil, err
	}

	method := strings.ToUpper(r.Method)

	query := emptyQuery
	if r.URL.RawQuery != "" {
		query = r.URL.Query()
	}

	var rawBody []byte

	if expectBody(r.Method, path) || checkExtraExpectedReqBodyPaths(extraPaths, r.Method, path) {
		var err error
		rawBody, err = util.ReadMaybeCompressedBody(r)
		if err != nil {
			return r, nil, err
		}
	}

	input := map[string]any{
		"path":    path,
		"method":  method,
		"params":  query,
		"headers": r.Header,
	}

	if len(rawBody) > 0 {
		var body any
		if expectYAML(r) {
			if err := util.Unmarshal(rawBody, &body); err != nil {
				return r, nil, err
			}
		} else if err := util.UnmarshalJSON(rawBody, &body); err != nil {
			return r, nil, err
		}

		// We cache the parsed body on the context so the server does not have
		// to parse the input document twice.
		input["body"] = body
		ctx := SetBodyOnContext(r.Context(), body)
		r = r.WithContext(ctx)
	}

	identity, ok := identifier.Identity(r)
	if ok {
		input["identity"] = identity
	}

	clientCertificates, ok := identifier.ClientCertificates(r)
	if ok {
		input["client_certificates"] = clientCertificates
	}

	return r, input, nil
}

var dataAPIVersions = map[string]bool{
	"v0": true,
	"v1": true,
}

func expectBody(method string, path []any) bool {
	if method == http.MethodPost {
		if len(path) == 1 {
			s := path[0].(string)
			return s == ""
		} else if len(path) >= 2 {
			s1 := path[0].(string)
			s2 := path[1].(string)
			return dataAPIVersions[s1] && s2 == "data"
		}
	}
	return false
}

func checkExtraExpectedReqBodyPaths(validators []func(string, []any) bool, method string, path []any) bool {
	for _, f := range validators {
		if f(method, path) {
			return true
		}
	}
	return false
}

func expectYAML(r *http.Request) bool {
	// NOTE(tsandall): This check comes from the server's HTTP handler code. The docs
	// are a bit more strict, but the authorizer should be consistent w/ the original
	// server handler implementation.
	return strings.Contains(r.Header.Get("Content-Type"), "yaml")
}

func parsePath(path string) ([]any, error) {
	if len(path) == 0 {
		return []any{}, nil
	}
	parts := strings.Split(path[1:], "/")
	for i := range parts {
		var err error
		parts[i], err = url.PathUnescape(parts[i])
		if err != nil {
			return nil, err
		}
	}
	sl := make([]any, len(parts))
	for i := range sl {
		sl[i] = parts[i]
	}
	return sl, nil
}

type authorizerCachedBody struct {
	parsed any
}

type authorizerCachedBodyKey string

const ctxkey authorizerCachedBodyKey = "authorizerCachedBodyKey"

// SetBodyOnContext adds the parsed input value to the context. This function is only
// exposed for test purposes.
func SetBodyOnContext(ctx context.Context, x any) context.Context {
	return context.WithValue(ctx, ctxkey, authorizerCachedBody{
		parsed: x,
	})
}

// GetBodyOnContext returns the parsed input from the request context if it exists.
// The authorizer saves the parsed input on the context when it runs.
func GetBodyOnContext(ctx context.Context) (any, bool) {
	input, ok := ctx.Value(ctxkey).(authorizerCachedBody)
	if !ok {
		return nil, false
	}
	return input.parsed, true
}
