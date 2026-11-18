package dashboardauth

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// RealmProxy returns an HTTP handler that forwards browser requests for the
// Keycloak realm and its static login assets to the cluster-internal issuer
// when the issuer origin differs from the browser-facing dashboard origin. It
// returns nil when the browser reaches the issuer directly on the same origin.
func (a *Auth) RealmProxy() http.Handler {
	if a.issuerBase == "" || a.browserBase == "" || a.issuerBase == a.browserBase {
		return nil
	}
	issuerURL, err := url.Parse(a.issuerBase)
	if err != nil {
		return nil
	}
	browserURL, err := url.Parse(a.browserBase)
	if err != nil {
		return nil
	}
	proxy := &httputil.ReverseProxy{
		Transport: a.proxyTransport(),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = issuerURL.Scheme
			pr.Out.URL.Host = issuerURL.Host
			pr.Out.Host = issuerURL.Host
			pr.Out.Header.Del("Accept-Encoding")
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") || strings.EqualFold(name, "Forwarded") {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Header.Set("X-Forwarded-Host", browserURL.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", browserURL.Scheme)
		},
		ModifyResponse: a.modifyProxyResponse,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !a.allowedProxyPath(r.URL.Path) {
			http.NotFound(w, r)
			return
		}
		proxy.ServeHTTP(w, r)
	})
}

// proxyTransport returns a RoundTripper with automatic gzip requests disabled
// so upstream Keycloak returns uncompressed text responses that can be
// rewritten safely.
func (a *Auth) proxyTransport() http.RoundTripper {
	if a.client != nil && a.client.Transport != nil {
		if tr, ok := a.client.Transport.(*http.Transport); ok {
			cloned := tr.Clone()
			cloned.DisableCompression = true
			return cloned
		}
		return a.client.Transport
	}
	if tr, ok := http.DefaultTransport.(*http.Transport); ok {
		cloned := tr.Clone()
		cloned.DisableCompression = true
		return cloned
	}
	return http.DefaultTransport
}

// browserAuthURL rewrites the authorization endpoint's scheme and host to the
// browser-facing dashboard origin when the realm is proxied through the
// dashboard listener.
func (a *Auth) browserAuthURL(raw string) string {
	if a.issuerBase == "" || a.browserBase == "" || a.issuerBase == a.browserBase {
		return raw
	}
	if rest, ok := strings.CutPrefix(raw, a.issuerBase); ok {
		return a.browserBase + rest
	}
	return raw
}

// allowedProxyPath reports whether the path is a safe Keycloak realm or static
// asset route. Administrative, master-realm, and non-realm paths are refused.
func (a *Auth) allowedProxyPath(rawPath string) bool {
	if rawPath == "" || strings.Contains(rawPath, "..") {
		return false
	}
	cleaned := path.Clean(rawPath)
	if strings.HasPrefix(cleaned, "/resources/") && strings.HasPrefix(rawPath, "/resources/") {
		return true
	}
	realmPrefix := "/realms/"
	if strings.HasPrefix(a.realmPath, "/realms/") {
		realmPrefix = a.realmPath + "/"
		if cleaned == a.realmPath && rawPath == a.realmPath {
			return true
		}
	}
	return strings.HasPrefix(cleaned, realmPrefix) && strings.HasPrefix(rawPath, realmPrefix)
}

// modifyProxyResponse rewrites upstream issuer URLs in Location headers and
// text bodies to the browser-facing dashboard origin, and strips Secure cookie
// attributes when the dashboard is served over plain HTTP.
func (a *Auth) modifyProxyResponse(resp *http.Response) error {
	if rest, ok := strings.CutPrefix(resp.Header.Get("Location"), a.issuerBase); ok {
		resp.Header.Set("Location", a.browserBase+rest)
	}
	if !a.secure {
		cookies := resp.Header.Values("Set-Cookie")
		if len(cookies) > 0 {
			resp.Header.Del("Set-Cookie")
			for _, cookie := range cookies {
				resp.Header.Add("Set-Cookie", stripSecureCookieAttribute(cookie))
			}
		}
	}
	if resp.Body == nil || !rewritableContentType(resp.Header.Get("Content-Type")) {
		return nil
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return fmt.Errorf("read proxied realm response: %w", err)
	}
	rewritten := bytes.ReplaceAll(body, []byte(a.issuerBase), []byte(a.browserBase))
	resp.Body = io.NopCloser(bytes.NewReader(rewritten))
	resp.ContentLength = int64(len(rewritten))
	resp.Header.Del("Content-Length")
	resp.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return nil
}

// rewritableContentType reports whether the response body is a text document
// that can embed absolute Keycloak URLs.
func rewritableContentType(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "text/html", "text/css", "application/javascript", "text/javascript", "application/json":
		return true
	default:
		return false
	}
}

// stripSecureCookieAttribute removes the Secure attribute from a Set-Cookie
// header and relaxes SameSite=None to SameSite=Lax so browsers accept the
// cookie over a plain HTTP port-forward or Tailscale connection.
func stripSecureCookieAttribute(raw string) string {
	parts := strings.Split(raw, ";")
	kept := make([]string, 0, len(parts))
	for i, part := range parts {
		trimmed := strings.TrimSpace(part)
		if i > 0 && strings.EqualFold(trimmed, "Secure") {
			continue
		}
		if i > 0 && strings.EqualFold(trimmed, "SameSite=None") {
			kept = append(kept, "SameSite=Lax")
			continue
		}
		kept = append(kept, trimmed)
	}
	return strings.Join(kept, "; ")
}
