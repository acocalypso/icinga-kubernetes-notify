// icinga-kubernetes-notify watches the object states that Icinga for Kubernetes writes to
// its database and reports problems and recoveries to Icinga Notifications, which then
// delivers them through its channels (for example webhooks).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"icinga-kubernetes-notify/internal/notify"
)

func main() {
	configPath := flag.String("config", "/etc/icinga-kubernetes-notify/config.yml", "path to the configuration file")
	check := flag.Bool("check-config", false, "validate the configuration and exit")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	cfg, err := notify.LoadConfig(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(2)
	}
	if *check {
		fmt.Println("configuration is valid")
		return
	}

	svc, err := notify.NewService(cfg, log)
	if err != nil {
		log.Error("startup failed", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("starting icinga-kubernetes-notify",
		"kinds", cfg.Watch.Kinds, "interval", cfg.Watch.Interval, "for", cfg.Watch.For, "recover_after", cfg.Watch.RecoverAfter)
	if err := svc.Run(ctx); err != nil {
		log.Error("stopped with error", "error", err)
		os.Exit(1)
	}
}
