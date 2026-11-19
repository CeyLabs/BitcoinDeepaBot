package api

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"gorm.io/gorm"

	"github.com/gorilla/mux"
	log "github.com/sirupsen/logrus"
)

type Server struct {
	httpServer *http.Server
	router     *mux.Router
}

const (
	StatusError = "ERROR"
	StatusOk    = "OK"

	// ServerWriteTimeout bounds how long a handler has to produce a response.
	// A handler that overruns it has its write fail, leaving the caller with a
	// dropped connection instead of an answer — so any handler that waits on
	// something slow must budget against this (see sendToInvoice).
	ServerWriteTimeout = 90 * time.Second

	// responseWriteMargin is held back from ServerWriteTimeout so there is
	// always room to serialise and flush the response.
	responseWriteMargin = 10 * time.Second
)

func NewServer(address string) *Server {
	srv := &http.Server{
		Addr: address,
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: ServerWriteTimeout,
		ReadTimeout:  ServerWriteTimeout,
	}
	apiServer := &Server{
		httpServer: srv,
	}
	apiServer.router = mux.NewRouter()
	apiServer.httpServer.Handler = apiServer.router
	return apiServer
}

// ListenAndServe starts serving in the background. Call it after all
// middleware and routes are added: the router must not change while it serves.
func (w *Server) ListenAndServe() {
	go w.httpServer.ListenAndServe()
	log.Infof("[api] Server started at %s", w.httpServer.Addr)
}
func (w *Server) PathPrefix(path string, handler http.Handler) {
	w.router.PathPrefix(path).Handler(handler)
}
func (w *Server) AppendAuthorizedRoute(path string, authType AuthType, accessType AccessKeyType, database *gorm.DB, handler func(http.ResponseWriter, *http.Request), methods ...string) {
	r := w.router.HandleFunc(path, LoggingMiddleware("API", AuthorizationMiddleware(database, authType, accessType, handler)))
	if len(methods) > 0 {
		r.Methods(methods...)
	}
}

// Use adds middleware that runs on every request to this server, before
// routing. Unlike mux's router.Use it also runs on 404 and 405 answers, so a
// blocked caller cannot map routes. Call it before ListenAndServe.
func (w *Server) Use(mw mux.MiddlewareFunc) {
	w.httpServer.Handler = mw(w.httpServer.Handler)
}

// BearerTokenMiddleware requires "Authorization: Bearer <token>" on every
// request, compared in constant time. An empty token disables the check.
func BearerTokenMiddleware(token string) mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		if token == "" {
			return next
		}
		want := []byte("Bearer " + token)
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
				log.Warnf("[api] Rejected unauthenticated admin request %s %s from %s", r.Method, r.URL.Path, r.RemoteAddr)
				http.Error(rw, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(rw, r)
		})
	}
}

func (w *Server) AppendRoute(path string, handler func(http.ResponseWriter, *http.Request), methods ...string) {
	r := w.router.HandleFunc(path, LoggingMiddleware("API", handler))
	if len(methods) > 0 {
		r.Methods(methods...)
	}
}

func NotFoundHandler(writer http.ResponseWriter, err error) {
	log.Errorln(err)
	// return 404 on any error
	http.Error(writer, "404 page not found", http.StatusNotFound)
}

func WriteResponse(writer http.ResponseWriter, response interface{}) error {
	jsonResponse, err := json.Marshal(response)
	if err != nil {
		return err
	}
	_, err = writer.Write(jsonResponse)
	return err
}
