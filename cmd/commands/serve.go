package commands

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/ardanlabs/conf/v2"
	"github.com/gisquick/gisquick-server/internal/application"
	"github.com/gisquick/gisquick-server/internal/infrastructure/email"
	"github.com/gisquick/gisquick-server/internal/infrastructure/postgres"
	"github.com/gisquick/gisquick-server/internal/infrastructure/project"
	"github.com/gisquick/gisquick-server/internal/infrastructure/security"
	"github.com/gisquick/gisquick-server/internal/infrastructure/ws"
	"github.com/gisquick/gisquick-server/internal/mock"
	"github.com/gisquick/gisquick-server/internal/server"
	"github.com/gisquick/gisquick-server/internal/server/auth"
	"github.com/go-redis/redis/v8"
	"go.uber.org/zap"
)

func Serve(log *zap.SugaredLogger) error {
	cfg := struct {
		Gisquick struct {
			Debug        bool   `conf:"default:false"`
			ProjectsRoot string `conf:"default:/publish"`
			MapCacheRoot string
			MapserverURL string
		}
		Auth struct {
			SessionExpiration time.Duration `conf:"default:12h"`
			SecretKey         string        `conf:"default:secret-key,mask"`
		}
		Web struct {
			ReadTimeout     time.Duration `conf:"default:5s"`
			WriteTimeout    time.Duration `conf:"default:10s"`
			IdleTimeout     time.Duration `conf:"default:120s"`
			ShutdownTimeout time.Duration `conf:"default:20s"`
			SiteURL         string        `conf:"default:http://localhost"`
			APIHost         string        `conf:"default:0.0.0.0:3000"`
			// DebugHost       string        `conf:"default:0.0.0.0:4000"`
		}
		Postgres struct {
			User         string `conf:"default:postgres"`
			Password     string `conf:"default:postgres,mask"`
			Host         string `conf:"default:postgres"`
			Name         string `conf:"default:postgres,env:POSTGRES_DB"`
			MaxIdleConns int    `conf:"default:3"`
			MaxOpenConns int    `conf:"default:3"`
			DisableTLS   bool   `conf:"default:true"`
		}
		Redis struct {
			Addr     string `conf:"default:redis:6379"` // "/var/run/redis/redis.sock"
			Network  string // "unix"
			Password string `conf:"mask"`
			DB       int    `conf:"default:0"`
		}
		Email struct {
			Host     string
			Port     int  `conf:"default:465"`
			SSL      bool `conf:"default:true"`
			Username string
			Password string `conf:"mask"`
			Sender   string
		}
	}{}

	// const prefix = "GISQUICK"
	const prefix = ""
	help, err := conf.Parse(prefix, &cfg)
	if err != nil {
		if errors.Is(err, conf.ErrHelpWanted) {
			fmt.Println(help)
			return nil
		}
		return fmt.Errorf("parsing config: %w", err)
	}
	out, err := conf.String(&cfg)
	if err != nil {
		return fmt.Errorf("generating config for output: %w", err)
	}
	// fmt.Println(out)
	log.Infow("startup", "config", out)

	// Database
	dbConn, err := server.OpenDB(server.DBConfig{
		User:         cfg.Postgres.User,
		Password:     cfg.Postgres.Password,
		Host:         cfg.Postgres.Host,
		Name:         cfg.Postgres.Name,
		MaxIdleConns: cfg.Postgres.MaxIdleConns,
		MaxOpenConns: cfg.Postgres.MaxOpenConns,
		DisableTLS:   cfg.Postgres.DisableTLS,
	})
	if err != nil {
		return fmt.Errorf("connecting to db: %w", err)
	}
	defer func() {
		// log.Infow("shutdown", "status", "stopping database support", "host", cfg.Postgres.Host)
		dbConn.Close()
	}()

	// for unix socket, use Network: "unix" and Addr: "/var/run/redis/redis.sock"
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.Redis.Addr,
		Network:  cfg.Redis.Network,
		Password: cfg.Redis.Password,
		DB:       cfg.Redis.DB,
	})
	defer rdb.Close()

	// es := &email.EmailService{
	// 	Host:     cfg.Email.Host,
	// 	Port:     cfg.Email.Port,
	// 	SSL:      cfg.Email.SSL,
	// 	Username: cfg.Email.Username,
	// 	Password: cfg.Email.Password,
	// }
	es := mock.NewDummyEmailService()

	conf := server.Config{
		MapserverURL: cfg.Gisquick.MapserverURL,
		MapCacheRoot: cfg.Gisquick.MapCacheRoot,
		ProjectsRoot: cfg.Gisquick.ProjectsRoot,
		SiteURL:      cfg.Web.SiteURL,
	}

	// Services
	accountsRepo := postgres.NewAccountsRepository(dbConn)
	tokenGenerator := security.NewTokenGenerator(cfg.Auth.SecretKey, "signup", cfg.Auth.SessionExpiration)
	emailSender := email.NewAccountsEmailSender(es, cfg.Email.Sender, cfg.Web.SiteURL)
	accountsService := application.NewAccountsService(emailSender, accountsRepo, tokenGenerator)

	siteURL, err := url.Parse(cfg.Web.SiteURL)
	if err != nil {
		return fmt.Errorf("invalid SiteURL value: %s", cfg.Web.SiteURL)
	}
	sessionStore := auth.NewRedisStore(rdb)
	authServ := auth.NewAuthService(log, siteURL.Hostname(), cfg.Auth.SessionExpiration, accountsRepo, sessionStore)

	projectsRepo2 := project.NewDiskStorage(log, filepath.Join(cfg.Gisquick.ProjectsRoot))
	projectsServV2 := application.NewProjectsService(log, projectsRepo2)

	sws := ws.NewSettingsWS(log)
	s := server.NewServer(log, conf, authServ, accountsService, projectsServV2, sws)

	// Start server
	go func() {
		if err := s.ListenAndServe(cfg.Web.APIHost); err != nil && err != http.ErrServerClosed {
			log.Fatalf("shutting down the server: %v", err)
		}
	}()
	// Wait for interrupt signal to gracefully shutdown the server with a timeout of 10 seconds.
	// Use a buffered channel to avoid missing signals as recommended for signal.Notify
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt)
	<-quit
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
	return nil
}