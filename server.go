package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"boot.dev/linko/internal/store"
)

// httpRequestsTotal counts requests by method, path and status.
var httpRequestsTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total number of HTTP requests.",
	},
	[]string{"method", "path", "status"},
)

type server struct {
	httpServer *http.Server
	store      store.Store
	cancel     context.CancelFunc
	logger     *slog.Logger
}

func newServer(store store.Store, port int, cancel context.CancelFunc, logger *slog.Logger) *server {
	mux := http.NewServeMux()

	s := &server{
		store:  store,
		cancel: cancel,
		logger: logger,
	}

	s.httpServer = &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: metricsMiddleware(requestIDMiddleware()(requestLogger(s.logger)(mux))),
	}

	mux.HandleFunc("GET /", s.handlerIndex)
	mux.Handle("POST /api/login", s.authMiddleware(http.HandlerFunc(s.handlerLogin)))
	mux.Handle("POST /api/shorten", s.authMiddleware(http.HandlerFunc(s.handlerShortenLink)))
	mux.Handle("GET /api/stats", s.authMiddleware(http.HandlerFunc(s.handlerStats)))
	mux.Handle("GET /api/urls", s.authMiddleware(http.HandlerFunc(s.handlerListURLs)))
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /{shortCode}", s.handlerRedirect)
	mux.HandleFunc("POST /admin/shutdown", s.handlerShutdown)

	return s
}

func (s *server) start() error {
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	if err := s.httpServer.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	// Print the server running message after the server has started successfully.
	tcpAddr, ok := ln.Addr().(*net.TCPAddr)
	if ok {
		s.logger.Debug("Linko is running",
			"url", fmt.Sprintf("http://localhost:%d", tcpAddr.Port),
		)
	}
	return nil
}

const logContextKey contextKey = "log_context"

type LogContext struct {
	Username string
	Error    error
}

func (s *server) shutdown(ctx context.Context) error {
	s.logger.Debug("Linko is shutting down")
	return s.httpServer.Shutdown(ctx)
}

type spyReadCloser struct {
	io.ReadCloser
	bytesRead int
}

func (s *spyReadCloser) Read(p []byte) (int, error) {
	n, err := s.ReadCloser.Read(p)
	s.bytesRead += n
	return n, err
}

type spyResponseWriter struct {
	http.ResponseWriter
	bytesWritten int
	statusCode   int
}

func (w *spyResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytesWritten += n
	return n, err
}

func (w *spyResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func requestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now() // Get start time of the request
			spyReader := &spyReadCloser{ReadCloser: r.Body}
			r.Body = spyReader
			spyWriter := &spyResponseWriter{ResponseWriter: w}
			// Create a *LogContext and store it on the request with r.WithContext and context.WithValue
			logCtx := &LogContext{}
			r = r.WithContext(context.WithValue(r.Context(), logContextKey, logCtx))
			next.ServeHTTP(spyWriter, r)
			attrs := []any{
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("client_ip", redactIP(r.RemoteAddr)),
				slog.String("request_id", spyWriter.Header().Get("X-Request-ID")),
				slog.Int("request_body_bytes", spyReader.bytesRead),
				slog.Int("response_body_bytes", spyWriter.bytesWritten),
				slog.Int("response_status", spyWriter.statusCode),
				slog.Duration("duration", time.Since(start)),
			}
			if logCtx.Error != nil {
				attrs = append(attrs, slog.Any("error", logCtx.Error))
			}
			if logCtx.Username != "" {
				attrs = append(attrs, slog.String("user", logCtx.Username))
			}
			logger.Info("Served request", attrs...)
		})
	}
}

func requestIDMiddleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-ID")
			if requestID == "" {
				requestID = rand.Text()
			}
			w.Header().Set("X-Request-ID", requestID)
			next.ServeHTTP(w, r)
		})
	}
}

func (s *server) handlerShutdown(w http.ResponseWriter, r *http.Request) {
	if os.Getenv("ENV") == "production" {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	go s.cancel()
}

func httpError(ctx context.Context, w http.ResponseWriter, status int, err error) {
	if logCtx, ok := ctx.Value(logContextKey).(*LogContext); ok {
		logCtx.Error = err
	}
	if status == 401 || status == 403 || status == 500 {
		http.Error(w, http.StatusText(status), status)
	} else {
		http.Error(w, err.Error(), status)
	}
}

func redactIP(addr string) string {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	parsedIP := net.ParseIP(host)
	if parsedIP.To4() != nil {
		parts := strings.Split(host, ".")
		if len(parts) == 4 {
			parts[3] = "x"
			host = strings.Join(parts, ".")
		}
		return host
	}
	return addr
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{
			ResponseWriter: w,
			status:         http.StatusOK,
		}

		next.ServeHTTP(rec, r)

		path := r.URL.Path
		method := r.Method
		status := strconv.Itoa(rec.status)

		httpRequestsTotal.
			WithLabelValues(method, path, status).
			Inc()
	})
}
