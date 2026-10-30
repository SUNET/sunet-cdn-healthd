// sunet-cdn-healthd answers health checks from the l4lb nodes on cache nodes,
// using the IETF health check format (draft-inadarei-api-health-check).
package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/hlog"
)

// version set at build time with -ldflags="-X main.version=v0.0.1"
var version = "undefined"

// tunnelInterfaces receive the ipip traffic from the l4lb nodes. If one is
// missing or down, no tenant on this node gets that address family's traffic.
var tunnelInterfaces = []string{"tunl0", "ip6tnl0"}

const (
	listenAddress = ":8088"
	checkInterval = time.Second
)

func runChecks(checkFuncs ...func() error) error {
	var errs []error
	for _, checkFunc := range checkFuncs {
		errs = append(errs, checkFunc())
	}

	return errors.Join(errs...)
}

func checkTunnels() error {
	for _, name := range tunnelInterfaces {
		iface, err := net.InterfaceByName(name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if iface.Flags&net.FlagUp == 0 {
			return fmt.Errorf("%s: interface is down", name)
		}
	}
	return nil
}

// result is a ready-to-send /healthz answer.
type result struct {
	code int
	body []byte
	err  error
}

func newResult(err error) (*result, error) {
	type health struct {
		Status string `json:"status"`
		Output string `json:"output,omitempty"`
	}

	h, code := health{Status: "pass"}, http.StatusOK

	if err != nil {
		h = health{
			Status: "fail",
			// errors.Join() in runChecks() uses newlines, make it
			// comma separated instead
			Output: strings.ReplaceAll(err.Error(), "\n", ", "),
		}
		code = http.StatusServiceUnavailable
	}

	body, marshalErr := json.Marshal(h)
	if marshalErr != nil {
		return nil, fmt.Errorf("unable to marshal health response: %w", marshalErr)
	}

	return &result{code: code, body: body, err: err}, nil
}

// stdErrorLog sends net/http's internal error messages through zerolog.
func stdErrorLog(logger zerolog.Logger) *log.Logger {
	return log.New(logger.With().Str("component", "net/http").Logger(), "", 0)
}

func main() {
	// Use short filenames for Caller() info, like sunet-cdn-manager.
	zerolog.CallerMarshalFunc = func(_ uintptr, file string, line int) string {
		return filepath.Base(file) + ":" + strconv.Itoa(line)
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "sunet-cdn-healthd-hostname-unknown"
	}

	logger := zerolog.New(os.Stderr).Level(zerolog.InfoLevel).With().
		Str("service", "sunet-cdn-healthd").
		Str("hostname", hostname).
		Str("server_version", version).
		Str("go_version", runtime.Version()).
		Timestamp().Caller().Logger()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	checks := []func() error{
		checkTunnels,
	}

	// Check once before serving, so the first answer is a real one.
	initial, err := newResult(
		runChecks(
			checks...,
		),
	)
	if err != nil {
		logger.Fatal().Err(err).Msg("initial health check failed")
	}
	var state atomic.Pointer[result]
	state.Store(initial)
	if initial.err != nil {
		logger.Warn().Err(initial.err).Msg("health check failing")
	}

	// Check in the background so the handler never waits on a check.
	go func() {
		ticker := time.NewTicker(checkInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				next, err := newResult(
					runChecks(
						checks...,
					),
				)
				if err != nil {
					logger.Err(err).Msg("unable to update health state")
					continue
				}
				prev := state.Swap(next)
				// Log changes, not every check.
				if prev.code != next.code {
					if next.err != nil {
						logger.Warn().Err(next.err).Msg("health check failing")
					} else {
						logger.Info().Msg("health check passing")
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		r := state.Load()
		w.Header().Set("Content-Type", "application/health+json")
		w.WriteHeader(r.code)
		_, err = w.Write(r.body)
		if err != nil {
			logger.Err(err).Msg("error writing response to /healthz request")
		}
	})

	middlewares := []func(http.Handler) http.Handler{
		hlog.NewHandler(logger),
		hlog.AccessHandler(func(r *http.Request, status, size int, duration time.Duration) {
			hlog.FromRequest(r).Info().
				Str("method", r.Method).
				Stringer("url", r.URL).
				Int("status", status).
				Int("size", size).
				Dur("duration", duration).
				Msg("")
		}),
		hlog.RemoteAddrHandler("ip"),
		hlog.UserAgentHandler("user_agent"),
		hlog.RequestIDHandler("req_id", "Request-Id"),
	}
	// Wrap from the innermost middleware out, so the list reads outermost
	// first like chi.Chain in sunet-cdn-manager.
	var handler http.Handler = mux
	for _, mw := range slices.Backward(middlewares) {
		handler = mw(handler)
	}

	srv := &http.Server{
		Addr:              listenAddress,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
		ErrorLog:          stdErrorLog(logger),
	}

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		logger.Info().Msg("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Err(err).Msg("unable to shut down cleanly")
		}
	}()

	logger.Info().Str("address", listenAddress).Msg("serving /healthz")
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		logger.Fatal().Err(err).Msg("server failed")
	}
	<-shutdownDone
}
