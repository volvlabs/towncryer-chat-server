/******************************************************************************
 *
 *  Description :
 *
 *  Web server initialization and shutdown.
 *
 *****************************************************************************/

package main

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/volvlabs/towncryer-chat-server/server/logs"
	"github.com/volvlabs/towncryer-chat-server/server/store"
	"github.com/volvlabs/towncryer-chat-server/server/store/types"
)

func listenAndServe(addr string, mux *http.ServeMux, tlfConf *tls.Config, stop <-chan bool) error {
	globals.shuttingDown = false

	httpdone := make(chan bool)

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second,
		MaxHeaderBytes:    1 << 14,
	}

	server.TLSConfig = tlfConf

	go func() {
		var err error
		if server.TLSConfig != nil {
			// If port is not specified, use default https port (443),
			// otherwise it will default to 80
			if addr == "" {
				addr = ":https"
			}

			if globals.tlsRedirectHTTP != "" {
				// Serving redirects from a unix socket or to a unix socket makes no sense.
				if isUnixAddr(globals.tlsRedirectHTTP) || isUnixAddr(addr) {
					err = errors.New("HTTP to HTTPS redirect: unix sockets not supported")
				} else {
					logs.Info.Printf("Redirecting connections from HTTP at [%s] to HTTPS at [%s]",
						globals.tlsRedirectHTTP, addr)

					// This is a second HTTP server listenning on a different port.
					go func() {
						if err := http.ListenAndServe(globals.tlsRedirectHTTP, tlsRedirect(addr)); err != nil && err != http.ErrServerClosed {
							logs.Info.Println("HTTP redirect failed:", err)
						}
					}()
				}
			}

			if err == nil {
				logs.Info.Printf("Listening for client HTTPS connections on [%s]", addr)
				var lis net.Listener
				lis, err = netListener(addr)
				if err == nil {
					err = server.ServeTLS(lis, "", "")
				}
			}
		} else {
			logs.Info.Printf("Listening for client HTTP connections on [%s]", addr)
			var lis net.Listener
			lis, err = netListener(addr)
			if err == nil {
				err = server.Serve(lis)
			}
		}

		if err != nil {
			if globals.shuttingDown {
				logs.Info.Println("HTTP server: stopped")
			} else {
				logs.Err.Println("HTTP server: failed", err)
			}
		}
		httpdone <- true
	}()

	// Wait for either a termination signal or an error
Loop:
	for {
		select {
		case <-stop:
			// Flip the flag that we are terminating and close the Accept-ing socket, so no new connections are possible.
			globals.shuttingDown = true
			// Give server 2 seconds to shut down.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if err := server.Shutdown(ctx); err != nil {
				// failure/timeout shutting down the server gracefully
				logs.Err.Println("HTTP server failed to terminate gracefully", err)
			}

			// While the server shuts down, termianate all sessions.
			globals.sessionStore.Shutdown()

			// Wait for http server to stop Accept()-ing connections.
			<-httpdone
			cancel()

			// Shutdown local cluster node, if it's a part of a cluster.
			globals.cluster.shutdown()

			// Terminate plugin connections.
			pluginsShutdown()

			// Shutdown gRPC server, if one is configured.
			if globals.grpcServer != nil {
				// GracefulStop does not terminate ServerStream. Must use Stop().
				globals.grpcServer.Stop()
			}

			// Stop publishing statistics.
			statsShutdown()

			// Shutdown the hub. The hub will shutdown topics.
			hubdone := make(chan bool)
			globals.hub.shutdown <- hubdone

			// Wait for the hub to finish.
			<-hubdone

			// Stop updating users cache
			usersShutdown()

			break Loop

		case <-httpdone:
			break Loop
		}
	}
	return nil
}

func signalHandler() <-chan bool {
	stop := make(chan bool)

	signchan := make(chan os.Signal, 1)
	signal.Notify(signchan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		// Wait for a signal. Don't care which signal it is
		sig := <-signchan
		logs.Info.Printf("Signal received: '%s', shutting down", sig)
		stop <- true
	}()

	return stop
}

// Wrapper for http.Handler which optionally adds a Strict-Transport-Security to the response.
func hstsHandler(handler http.Handler) http.Handler {
	if globals.tlsStrictMaxAge != "" {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Strict-Transport-Security", "max-age="+globals.tlsStrictMaxAge)
			handler.ServeHTTP(w, r)
		})
	}
	return handler
}

// The following code is used to intercept HTTP errors so they can be wrapped into json.

// Wrapper around http.ResponseWriter which detects status set to 400+ and replaces
// default error message with a custom one.
type errorResponseWriter struct {
	status int
	http.ResponseWriter
}

func (w *errorResponseWriter) WriteHeader(status int) {
	if status >= http.StatusBadRequest {
		// charset=utf-8 is the default. No need to write it explicitly
		// Must set all the headers before calling super.WriteHeader()
		w.ResponseWriter.Header().Set("Content-Type", "application/json")
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *errorResponseWriter) Write(p []byte) (n int, err error) {
	if w.status >= http.StatusBadRequest {
		p, _ = json.Marshal(
			&ServerComMessage{
				Ctrl: &MsgServerCtrl{
					Timestamp: time.Now().UTC().Round(time.Millisecond),
					Code:      w.status,
					Text:      http.StatusText(w.status),
				},
			})
	}
	return w.ResponseWriter.Write(p)
}

// Handler which deploys errorResponseWriter
func httpErrorHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(&errorResponseWriter{0, w}, r)
		})
}

// Custom 404 response.
func serve404(wrt http.ResponseWriter, req *http.Request) {
	wrt.Header().Set("Content-Type", "application/json; charset=utf-8")
	wrt.WriteHeader(http.StatusNotFound)
	json.NewEncoder(wrt).Encode(
		&ServerComMessage{
			Ctrl: &MsgServerCtrl{
				Timestamp: time.Now().UTC().Round(time.Millisecond),
				Code:      http.StatusNotFound,
				Text:      "not found",
			},
		})
}

// Redirect HTTP requests to HTTPS
func tlsRedirect(toPort string) http.HandlerFunc {
	if toPort == ":443" || toPort == ":https" {
		toPort = ""
	} else if toPort != "" && toPort[:1] == ":" {
		// Strip leading colon. JoinHostPort will add it back.
		toPort = toPort[1:]
	}

	return func(wrt http.ResponseWriter, req *http.Request) {
		host, _, err := net.SplitHostPort(req.Host)
		if err != nil {
			// If SplitHostPort has failed assume it's because :port part is missing.
			host = req.Host
		}

		target, _ := url.ParseRequestURI(req.RequestURI)
		target.Scheme = "https"

		// Ensure valid redirect target.
		if toPort != "" {
			// Replace the port number.
			target.Host = net.JoinHostPort(host, toPort)
		} else {
			target.Host = host
		}

		if target.Path == "" {
			target.Path = "/"
		}

		http.Redirect(wrt, req, target.String(), http.StatusTemporaryRedirect)
	}
}

// Wrapper for http.Handler which optionally adds a Cache-Control header to the response
func cacheControlHandler(maxAge int, handler http.Handler) http.Handler {
	if maxAge > 0 {
		strMaxAge := strconv.Itoa(maxAge)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "must-revalidate, public, max-age="+strMaxAge)
			handler.ServeHTTP(w, r)
		})
	}
	return handler
}

// Get API key from an HTTP request.
func getAPIKey(req *http.Request) string {
	// Check header.
	apikey := req.Header.Get("X-Tinode-APIKey")
	if apikey != "" {
		return apikey
	}

	// Check URL query parameters.
	apikey = req.URL.Query().Get("apikey")
	if apikey != "" {
		return apikey
	}

	// Check form values.
	apikey = req.FormValue("apikey")
	if apikey != "" {
		return apikey
	}

	// Check cookies.
	if c, err := req.Cookie("apikey"); err == nil {
		apikey = c.Value
	}

	return apikey
}

// Extracts authorization credentials from an HTTP request.
// Returns authentication method and secret.
func getHttpAuth(req *http.Request) (method, secret string) {
	// Check X-Tinode-Auth header.
	if parts := strings.Split(req.Header.Get("X-Tinode-Auth"), " "); len(parts) == 2 {
		method, secret = parts[0], parts[1]
		return
	}

	// Check canonical Authorization header.
	if parts := strings.Split(req.Header.Get("Authorization"), " "); len(parts) == 2 {
		method, secret = parts[0], parts[1]
		return
	}

	// Check URL query parameters.
	if method = req.URL.Query().Get("auth"); method != "" {
		// Get the auth secret.
		secret = req.URL.Query().Get("secret")
		// Convert base64 URL-encoding to standard encoding.
		secret = strings.NewReplacer("-", "+", "_", "/").Replace(secret)
		return
	}

	// Check form values.
	if method = req.FormValue("auth"); method != "" {
		return method, req.FormValue("secret")
	}

	// Check cookies as the last resort.
	if mcookie, err := req.Cookie("auth"); err == nil {
		if scookie, err := req.Cookie("secret"); err == nil {
			method, secret = mcookie.Value, scookie.Value
		}
	}

	return
}

// Authenticate non-websocket HTTP request
func authHttpRequest(req *http.Request) (types.Uid, []byte, error) {
	var uid types.Uid
	if authMethod, secret := getHttpAuth(req); authMethod != "" {
		decodedSecret := make([]byte, base64.StdEncoding.DecodedLen(len(secret)))
		n, err := base64.StdEncoding.Decode(decodedSecret, []byte(secret))
		if err != nil {
			logs.Info.Println("media: invalid auth secret", authMethod, "'"+secret+"'")
			return uid, nil, types.ErrMalformed
		}

		if authhdl := store.Store.GetLogicalAuthHandler(authMethod); authhdl != nil {
			rec, challenge, err := authhdl.Authenticate(decodedSecret[:n], getRemoteAddr(req))
			if err != nil {
				return uid, nil, err
			}
			if challenge != nil {
				return uid, challenge, nil
			}
			uid = rec.Uid
		} else {
			logs.Info.Println("media: unknown auth method", authMethod)
		}
	} else {
		// Find the session, make sure it's appropriately authenticated.
		sess := globals.sessionStore.Get(req.FormValue("sid"))
		if sess != nil {
			uid = sess.uid
		}
	}
	return uid, nil, nil
}

// debugSession is session debug info.
type debugSession struct {
	RemoteAddr string   `json:"remote_addr,omitempty"`
	Ua         string   `json:"ua,omitempty"`
	Uid        string   `json:"uid,omitempty"`
	Sid        string   `json:"sid,omitempty"`
	Clnode     string   `json:"clnode,omitempty"`
	Subs       []string `json:"subs,omitempty"`
}

// debugTopic is a topic debug info.
type debugTopic struct {
	Topic    string   `json:"topic,omitempty"`
	Xorig    string   `json:"xorig,omitempty"`
	IsProxy  bool     `json:"is_proxy,omitempty"`
	PerUser  []string `json:"per_user,omitempty"`
	PerSubs  []string `json:"per_subs,omitempty"`
	Sessions []string `json:"sessions,omitempty"`
}

// debugCachedUser is a user cache entry debug info.
type debugCachedUser struct {
	Uid    string `json:"uid,omitempty"`
	Unread int    `json:"unread,omitempty"`
	Topics int    `json:"topics,omitempty"`
}

// debugDump is server internal state dump for debugging.
type debugDump struct {
	Version   string            `json:"server_version,omitempty"`
	Build     string            `json:"build_id,omitempty"`
	Timestamp time.Time         `json:"ts,omitempty"`
	Sessions  []debugSession    `json:"sessions,omitempty"`
	Topics    []debugTopic      `json:"topics,omitempty"`
	UserCache []debugCachedUser `json:"user_cache,omitempty"`
}

func serveStatus(wrt http.ResponseWriter, req *http.Request) {
	wrt.Header().Set("Content-Type", "application/json")

	result := &debugDump{
		Version:   currentVersion,
		Build:     buildstamp,
		Timestamp: types.TimeNow(),
		Sessions:  make([]debugSession, 0, len(globals.sessionStore.sessCache)),
		Topics:    make([]debugTopic, 0, 10),
		UserCache: make([]debugCachedUser, 0, 10),
	}
	// Sessions.
	globals.sessionStore.Range(func(sid string, s *Session) bool {
		keys := make([]string, 0, len(s.subs))
		for tn := range s.subs {
			keys = append(keys, tn)
		}
		sort.Strings(keys)
		var clnode string
		if s.clnode != nil {
			clnode = s.clnode.name
		}
		result.Sessions = append(result.Sessions, debugSession{
			RemoteAddr: s.remoteAddr,
			Ua:         s.userAgent,
			Uid:        s.uid.String(),
			Sid:        sid,
			Clnode:     clnode,
			Subs:       keys,
		})
		return true
	})
	// Topics.
	globals.hub.topics.Range(func(_, t any) bool {
		topic := t.(*Topic)
		psd := make([]string, 0, len(topic.sessions))
		for s := range topic.sessions {
			psd = append(psd, s.sid)
		}
		pud := make([]string, 0, len(topic.perUser))
		for uid := range topic.perUser {
			pud = append(pud, uid.String())
		}
		ps := make([]string, 0, len(topic.perSubs))
		for key := range topic.perSubs {
			ps = append(ps, key)
		}
		result.Topics = append(result.Topics, debugTopic{
			Topic:    topic.name,
			Xorig:    topic.xoriginal,
			IsProxy:  topic.isProxy,
			PerUser:  pud,
			PerSubs:  ps,
			Sessions: psd,
		})
		return true
	})
	for k, v := range usersCache {
		result.UserCache = append(result.UserCache, debugCachedUser{
			Uid:    k.UserId(),
			Unread: v.unread,
			Topics: v.topics,
		})
	}

	json.NewEncoder(wrt).Encode(result)
}
