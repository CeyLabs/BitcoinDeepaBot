package api

import (
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
	go apiServer.httpServer.ListenAndServe()
	log.Infof("[api] Server started at %s", address)
	return apiServer
}

func (w *Server) ListenAndServe() {
	go w.httpServer.ListenAndServe()
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
