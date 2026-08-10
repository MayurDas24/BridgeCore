package middleware

import "net/http"

// Middleware is the standard http middleware function shape.
type Middleware func(http.Handler) http.Handler

// Chain composes middlewares left-to-right so that Chain(a, b, c)(handler)
// executes as a(b(c(handler))) — i.e. 'a' is the outermost layer and runs
// first on the way in / last on the way out.
func Chain(mws ...Middleware) Middleware {
	return func(final http.Handler) http.Handler {
		h := final
		for i := len(mws) - 1; i >= 0; i-- {
			h = mws[i](h)
		}
		return h
	}
}
