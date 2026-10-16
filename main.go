package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

// OpenCode 1.x uses a 48-bit timestamp/counter followed by 14 random characters.
// Session timestamps descend; message timestamps ascend (v1.19.1 id/id.ts).
func clientID(prefix string) string {
	stamp := uint64(time.Now().UnixMilli())<<12 | 1
	if prefix == "ses" {
		stamp = ^stamp
	}
	return fmt.Sprintf("%s_%012x%s", prefix, stamp&0xffffffffffff, rand.Text()[:14])
}

func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message, "type": "api_error", "code": http.StatusText(status)}})
}

func handler(upstream *url.URL, localKey, version string) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.SetURL(upstream)
			r.Out.URL.Path = strings.TrimRight(upstream.Path, "/") + strings.TrimPrefix(r.In.URL.Path, "/v1")
			r.Out.URL.RawPath = ""
			if r.In.Context().Value(collectChatKey{}) == true {
				r.Out.Header.Del("Accept-Encoding")
			}
			// Authenticate locally only; upstream free-model requests carry no API key.
			r.Out.Header.Del("Authorization")
			r.Out.Header.Del("X-Api-Key")
			r.Out.Header.Del("X-Goog-Api-Key")
			r.Out.Header.Del("Cookie")
			r.Out.Header.Del("X-Real-IP")
			r.Out.Header.Del("Cf-Connecting-IP")
			r.Out.Header.Set("User-Agent", "opencode/"+version)
			r.Out.Header.Set("X-Opencode-Client", "cli")
			if r.Out.Header.Get("X-Opencode-Project") == "" {
				r.Out.Header.Set("X-Opencode-Project", "global")
			}
			session := r.Out.Header.Get("X-Opencode-Session")
			if session == "" {
				session = clientID("ses")
			}
			r.Out.Header.Set("X-Opencode-Session", session)
			r.Out.Header.Set("X-Opencode-Session-Id", session)
			r.Out.Header.Set("X-Opencode-Request", clientID("msg"))
		},
		ModifyResponse: func(r *http.Response) error {
			if r.Request.Context().Value(collectChatKey{}) == true && r.StatusCode == http.StatusOK {
				return collectChat(r)
			}
			return nil
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			log.Printf("upstream request failed: %v", err)
			apiError(w, http.StatusBadGateway, "OpenCode upstream is unavailable")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if localKey == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+localKey)) != 1 {
			apiError(w, http.StatusUnauthorized, "Invalid API key")
			return
		}
		method := ""
		switch r.URL.Path {
		case "/v1/models":
			method = http.MethodGet
		case "/v1/chat/completions", "/v1/responses":
			method = http.MethodPost
		default:
			apiError(w, http.StatusNotFound, "Unknown endpoint")
			return
		}
		if r.Method != method {
			w.Header().Set("Allow", method)
			apiError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		if r.URL.Path == "/v1/chat/completions" {
			collect, err := prepareChat(r)
			if err != nil {
				apiError(w, http.StatusBadRequest, err.Error())
				return
			}
			if collect {
				r = r.WithContext(context.WithValue(r.Context(), collectChatKey{}, true))
			}
		}
		proxy.ServeHTTP(w, r)
	})
}

func main() {
	localKey := os.Getenv("BRIDGE_API_KEY")
	if localKey == "" {
		log.Fatal("BRIDGE_API_KEY must be set")
	}
	upstream, err := url.Parse(env("OPENCODE_BASE_URL", "https://opencode.ai/zen/v1"))
	if err != nil || upstream.Host == "" || (upstream.Scheme != "http" && upstream.Scheme != "https") || upstream.User != nil || upstream.RawQuery != "" || upstream.Fragment != "" {
		log.Fatal("OPENCODE_BASE_URL must be an HTTP(S) URL without credentials, query or fragment")
	}
	server := &http.Server{
		Addr:              env("LISTEN_ADDR", "127.0.0.1:8080"),
		Handler:           handler(upstream, localKey, env("OPENCODE_CLIENT_VERSION", "1.19.1")),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	log.Printf("OpenAI-compatible server listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}
