package server

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc/reflection"

	"github.com/AleksK1NG/nats-streaming/config"
	grpc2 "github.com/AleksK1NG/nats-streaming/internal/email/delivery/grpc"
	"github.com/AleksK1NG/nats-streaming/internal/email/repository"
	"github.com/AleksK1NG/nats-streaming/internal/email/usecase"
	"github.com/AleksK1NG/nats-streaming/pkg/logger"
	emailService "github.com/AleksK1NG/nats-streaming/proto/email"
	"github.com/go-redis/redis/v8"
	grpc_middleware "github.com/grpc-ecosystem/go-grpc-middleware"
	"github.com/jackc/pgx/v4/pgxpool"
	"github.com/nats-io/stan.go"
	"github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	grpcrecovery "github.com/grpc-ecosystem/go-grpc-middleware/recovery"
	grpc_ctxtags "github.com/grpc-ecosystem/go-grpc-middleware/tags"
	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	grpc_prometheus "github.com/grpc-ecosystem/go-grpc-prometheus"

	"github.com/labstack/echo/v4"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	certFile        = "ssl/server.crt"
	keyFile         = "ssl/server.pem"
	maxHeaderBytes  = 1 << 20
	gzipLevel       = 5
	stackSize       = 1 << 10 // 1 KB
	csrfTokenHeader = "X-CSRF-Token"
	bodyLimit       = "2M"
	kafkaGroupID    = "products_group"
)

type server struct {
	log      logger.Logger
	cfg      *config.Config
	natsConn stan.Conn
	pgxPool  *pgxpool.Pool
	tracer   opentracing.Tracer
	echo     *echo.Echo
	redis    *redis.Client
}

// NewServer constructor
func NewServer(log logger.Logger, cfg *config.Config, natsConn stan.Conn, pgxPool *pgxpool.Pool, tracer opentracing.Tracer, redis *redis.Client) *server {
	return &server{log: log, cfg: cfg, natsConn: natsConn, pgxPool: pgxPool, tracer: tracer, redis: redis, echo: echo.New()}
}

// Run start application
func (s *server) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	emailPgRepo := repository.NewEmailPGRepository(s.pgxPool)
	emailUC := usecase.NewEmailUseCase(s.log, emailPgRepo)

	// validate := validator.New()

	go func() {
		s.log.Infof("Server is listening on PORT: %s", s.cfg.HTTP.Port)
		s.runHttpServer()
	}()

	metricsServer := echo.New()
	go func() {
		metricsServer.GET("/metrics", echo.WrapHandler(promhttp.Handler()))
		s.log.Infof("Metrics server is running on port: %s", s.cfg.Metrics.Port)
		if err := metricsServer.Start(s.cfg.Metrics.Port); err != nil {
			s.log.Error(err)
			cancel()
		}
	}()

	l, err := net.Listen("tcp", s.cfg.GRPC.Port)
	if err != nil {
		return errors.Wrap(err, "net.Listen")
	}
	defer l.Close()

	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		s.log.Fatalf("failed to load key pair: %s", err)
	}

	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewServerTLSFromCert(&cert)),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			MaxConnectionIdle: s.cfg.GRPC.MaxConnectionIdle * time.Minute,
			Timeout:           s.cfg.GRPC.Timeout * time.Second,
			MaxConnectionAge:  s.cfg.GRPC.MaxConnectionAge * time.Minute,
			Time:              s.cfg.GRPC.Timeout * time.Minute,
		}),
		grpc.UnaryInterceptor(grpc_middleware.ChainUnaryServer(
			grpc_ctxtags.UnaryServerInterceptor(),
			grpc_opentracing.UnaryServerInterceptor(),
			grpc_prometheus.UnaryServerInterceptor,
			grpcrecovery.UnaryServerInterceptor(),
			// im.Logger,
		),
		),
	)

	emailGRPCService := grpc2.NewEmailGRPCService(emailUC, s.log)
	emailService.RegisterEmailServiceServer(grpcServer, emailGRPCService)
	grpc_prometheus.Register(grpcServer)

	if s.cfg.HTTP.Development {
		reflection.Register(grpcServer)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)

	select {
	case v := <-quit:
		s.log.Errorf("signal.Notify: %v", v)
	case done := <-ctx.Done():
		s.log.Errorf("ctx.Done: %v", done)
	}

	if err := s.echo.Server.Shutdown(ctx); err != nil {
		return errors.Wrap(err, "echo.Server.Shutdown")
	}

	if err := metricsServer.Shutdown(ctx); err != nil {
		s.log.Errorf("metricsServer.Shutdown: %v", err)
	}

	grpcServer.GracefulStop()
	s.log.Info("Server Exited Properly")

	return nil
}