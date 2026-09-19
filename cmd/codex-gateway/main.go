package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/iaia/telegramgw/internal/admin"
	"github.com/iaia/telegramgw/internal/auth"
	"github.com/iaia/telegramgw/internal/buildinfo"
	"github.com/iaia/telegramgw/internal/config"
	"github.com/iaia/telegramgw/internal/gateway"
	"github.com/iaia/telegramgw/internal/registry"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	slog.SetDefault(logger)
	if err := run(os.Args[1:], logger); err != nil {
		logger.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}

func run(args []string, logger *slog.Logger) error {
	path := "gateway.json"
	for i := 0; i < len(args); i++ {
		if args[i] == "--config" {
			if i+1 >= len(args) {
				return errors.New("--config requires a file")
			}
			path = args[i+1]
			args = append(args[:i], args[i+2:]...)
			i--
		} else if strings.HasPrefix(args[i], "--config=") {
			path = strings.TrimPrefix(args[i], "--config=")
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}
	if len(args) == 0 {
		args = []string{"serve"}
	}
	if args[0] == "version" || args[0] == "--version" {
		fmt.Println(buildinfo.Version)
		return nil
	}
	if args[0] == "help" || args[0] == "--help" {
		fmt.Println("codex-gateway [--config gateway.json] serve|migrate|admin bootstrap|admin bootstrap-if-needed|webhook set|webhook status|menu set|menu status|worker create --name NAME|worker list|worker revoke ID|worker rotate-token ID")
		return nil
	}
	cfg, err := config.LoadGateway(path)
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	store, err := registry.Open(ctx, cfg.DatabasePath)
	if err != nil {
		return errors.New("could not open registry; check database_path and its directory permissions")
	}
	defer store.Close()
	migrateCtx, done := context.WithTimeout(ctx, 30*time.Second)
	err = store.Migrate(migrateCtx)
	done()
	if err != nil {
		return err
	}
	switch args[0] {
	case "menu":
		if len(args) != 2 || (args[1] != "set" && args[1] != "status") {
			return errors.New("use menu set or menu status")
		}
		api := gateway.NewTelegramClient(cfg.Secrets.BotToken)
		if args[1] == "set" {
			if err := gateway.SyncCommandMenu(ctx, api, cfg.Secrets.BotName); err != nil {
				return err
			}
			fmt.Println("Telegram command menu configured.")
			return nil
		}
		commands, err := api.GetMyCommands(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(commands)
	case "migrate":
		fmt.Println("Migrations applied.")
		return nil
	case "worker":
		return workerCommand(ctx, store, args[1:])
	case "admin":
		if len(args) != 2 || (args[1] != "bootstrap" && args[1] != "bootstrap-if-needed") {
			return errors.New("use admin bootstrap or admin bootstrap-if-needed")
		}
		return bootstrapGatewayAdmin(ctx, store, cfg.PublicBaseURL, args[1] == "bootstrap-if-needed", os.Stdout)
	case "webhook":
		if len(args) != 2 {
			return errors.New("use webhook set or webhook status")
		}
		api := gateway.NewTelegramClient(cfg.Secrets.BotToken)
		if args[1] == "status" {
			info, err := api.GetWebhook(ctx)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(info)
		}
		if args[1] != "set" {
			return errors.New("use webhook set or webhook status")
		}
		secret, err := webhookSecret(cfg)
		if err != nil {
			return err
		}
		identity, err := api.Identity(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(identity.Username, strings.TrimPrefix(cfg.Secrets.BotName, "@")) {
			return errors.New("Telegram token belongs to a different bot than BOTNAME")
		}
		if err := api.SetWebhook(ctx, cfg.PublicBaseURL+"/tgapi/v1/telegram/webhook", secret); err != nil {
			return err
		}
		fmt.Println("Telegram webhook configured.")
		return nil
	case "serve":
		if len(args) > 1 {
			return errors.New("unexpected serve arguments")
		}
		secret, err := webhookSecret(cfg)
		if err != nil {
			return err
		}
		hub := gateway.NewHub(store, logger, cfg.HeartbeatInterval, cfg.UnreachableAfter)
		hub.EventHandler = store.IngestEvent
		hub.AckHandler = store.AcknowledgeCommand
		mux := gateway.NewMux(store, hub)
		api := gateway.NewTelegramClient(cfg.Secrets.BotToken)
		mux.Handle("/tgapi/v1/telegram/webhook", gateway.NewWebhook(store, cfg, secret, api, logger))
		redactor, err := auth.NewRedactor([]string{regexp.QuoteMeta(cfg.Secrets.BotToken), regexp.QuoteMeta(secret), `cwk_[a-f0-9]+`, `sk-[A-Za-z0-9_-]{16,}`}, "[REDACTED]")
		if err != nil {
			return err
		}
		console, err := admin.New(store, admin.Config{Origin: cfg.PublicBaseURL, BotAPI: api,
			BotUsername: cfg.Secrets.BotName, AllowedUserCount: len(cfg.AllowedUserIDs),
			AllowedChatCount: len(cfg.AllowedChatIDs), Redactor: redactor})
		if err != nil {
			return err
		}
		mux.Handle("/tgadmin/", console)
		mux.Handle("/tgapi/v1/admin/", console)
		sender := gateway.NewSender(store, api, logger, gateway.SenderOptions{BotID: cfg.Secrets.BotName, OwnerID: cfg.Secrets.WLID, Redactor: redactor})
		dispatcher := gateway.NewDispatcher(store, hub, logger)
		server := &http.Server{Addr: cfg.Listen, Handler: mux, ReadHeaderTimeout: cfg.ReadHeaderTimeout, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
		serviceCtx, stopService := context.WithCancel(ctx)
		var services sync.WaitGroup
		services.Go(func() {
			menuCtx, cancel := context.WithTimeout(serviceCtx, 45*time.Second)
			defer cancel()
			if err := gateway.SyncCommandMenu(menuCtx, api, cfg.Secrets.BotName); err != nil && serviceCtx.Err() == nil {
				logger.Warn("Telegram command menu update failed; retry with codex-gateway menu set")
			}
		})
		services.Go(func() { hub.Run(serviceCtx) })
		services.Go(func() { _ = sender.Run(serviceCtx) })
		services.Go(func() { _ = dispatcher.Run(serviceCtx) })
		services.Go(func() { _ = gateway.NewPresence(store, api, logger).Run(serviceCtx) })
		defer func() { stopService(); services.Wait() }()
		errCh := make(chan error, 1)
		go func() { errCh <- server.ListenAndServe() }()
		logger.Info("gateway listening", "address", cfg.Listen, "version", buildinfo.Version)
		select {
		case err := <-errCh:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-ctx.Done():
			shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
			defer stop()
			return server.Shutdown(shutdown)
		}
	default:
		return errors.New("unknown command; use --help")
	}
}

func webhookSecret(cfg config.GatewayConfig) (string, error) {
	secret := os.Getenv(cfg.WebhookSecretEnv)
	if len(secret) < 32 || len(secret) > 256 || !regexp.MustCompile(`^[A-Za-z0-9_-]+$`).MatchString(secret) {
		return "", errors.New("webhook secret must contain 32..256 letters, digits, underscores or hyphens")
	}
	return secret, nil
}

func workerCommand(ctx context.Context, store *registry.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("worker subcommand required")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	switch args[0] {
	case "create":
		flags := flag.NewFlagSet("worker create", flag.ContinueOnError)
		name := flags.String("name", "", "Worker display name")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || flags.NArg() != 0 {
			return errors.New("worker create requires --name NAME")
		}
		token, err := auth.GenerateWorkerToken()
		if err != nil {
			return err
		}
		w, err := store.CreateWorker(ctx, registry.CreateWorkerInput{ID: uuid.New(), Name: *name, OS: "unknown", Arch: "unknown", TokenHash: auth.HashWorkerToken(token)})
		if err != nil {
			return err
		}
		fmt.Printf("Worker ID: %s\nToken: %s\n", w.ID, token)
		return nil
	case "list":
		workers, err := store.ListWorkers(ctx)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(workers)
	case "revoke", "rotate-token":
		if len(args) != 2 {
			return errors.New("worker command requires one worker UUID")
		}
		id, err := uuid.Parse(args[1])
		if err != nil {
			return errors.New("invalid worker UUID")
		}
		if args[0] == "revoke" {
			return store.RevokeWorker(ctx, id)
		}
		token, err := auth.GenerateWorkerToken()
		if err != nil {
			return err
		}
		if err = store.RotateWorkerToken(ctx, id, auth.HashWorkerToken(token)); err != nil {
			return err
		}
		fmt.Printf("Token: %s\n", token)
		return nil
	default:
		return errors.New("unknown worker command")
	}
}
