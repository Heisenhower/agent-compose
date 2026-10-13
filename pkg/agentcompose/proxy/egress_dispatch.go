package proxy

import "net/http"

// EgressDispatchHandler routes a guest's forward-proxy traffic to the egress
// proxy and everything else to the daemon's own handler, so the proxy is served
// on the listener a sandbox can already reach instead of a second port a
// deployment would have to publish.
//
// A request is treated as proxy traffic when it names an absolute target (the
// form only a proxy client produces) or when it is a CONNECT carrying a proxy
// credential. A CONNECT without one is left to the daemon: the daemon's own
// HTTP/2 clients may use the method, and the proxy answers an unauthenticated
// CONNECT with 407, which would turn that client's request into an
// authentication error. The credential is therefore what marks the request as
// the proxy's, and the proxy is the only producer of it.
func EgressDispatchHandler(daemon http.Handler, egress http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isEgressProxyRequest(r) {
			egress.ServeHTTP(w, r)
			return
		}
		daemon.ServeHTTP(w, r)
	})
}

func isEgressProxyRequest(r *http.Request) bool {
	if r.URL != nil && r.URL.IsAbs() {
		return true
	}
	return r.Method == http.MethodConnect && r.Header.Get("Proxy-Authorization") != ""
}
