package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
	supervisortelemetry "github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/telemetry"

	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
)

func registerOpAMPSupervisorCommand(root *cobra.Command) {
	var configPath string

	cmd := &cobra.Command{
		Use:   "otel-supervisor",
		Short: "Run the embedded OpAMP supervisor for Alloy's OTel engine",
		Long: "[EXPERIMENTAL] Run an embedded OpAMP supervisor that manages this same Alloy binary " +
			"as a supervised OpenTelemetry Collector agent (alloy otel), connecting to a remote " +
			"OpAMP server (e.g. Grafana Fleet Management) for remote configuration.\n\n" +
			"A supervisor configuration file is required via --config. It is authoritative and used " +
			"verbatim, exactly like a stock opampsupervisor config; only agent.executable is forced " +
			"to this Alloy binary.",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runOpAMPSupervisor(configPath)
		},
	}

	// --config is required; the file is authoritative and there is no env-driven mode.
	cmd.Flags().StringVar(&configPath, "config", "", "path to the authoritative supervisor configuration file (required)")
	_ = cmd.MarkFlagRequired("config")

	root.AddCommand(cmd)
}

func runOpAMPSupervisor(configPath string) error {
	cfg, err := loadSupervisorConfig(configPath)
	if err != nil {
		return err
	}

	logger, err := supervisortelemetry.NewLogger(cfg.Telemetry.Logs)
	if err != nil {
		return fmt.Errorf("failed to build supervisor logger: %w", err)
	}
	defer func() { _ = logger.Sync() }()

	runCtx := context.Background()

	sup, err := supervisor.NewSupervisor(runCtx, logger, cfg)
	if err != nil {
		return fmt.Errorf("failed to create supervisor: %w", err)
	}

	if err := sup.Start(runCtx); err != nil {
		return fmt.Errorf("failed to start supervisor: %w", err)
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	<-sigCtx.Done()
	stop() // restore default handling so a second signal is delivered to the force-quit waiter below

	logger.Info("Shutdown signal received; stopping supervisor gracefully (send the signal again to force-quit)")

	// Shutdown() can block for ~10-15s (agent SIGTERM -> 10s grace -> SIGKILL, plus server
	// teardown timeouts). If a second signal arrives, exit immediately.
	forceCtx, forceStop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer forceStop()
	go func() {
		<-forceCtx.Done()
		logger.Warn("Second shutdown signal received; forcing exit")
		_ = logger.Sync()
		os.Exit(1)
	}()

	sup.Shutdown()
	logger.Info("Supervisor stopped")
	return nil
}

func loadSupervisorConfig(configPath string) (config.Supervisor, error) {
	if configPath == "" {
		return config.Supervisor{}, fmt.Errorf("a supervisor configuration file is required (--config)")
	}

	resolverSettings := confmap.ResolverSettings{
		URIs: []string{configPath},
		ProviderFactories: []confmap.ProviderFactory{
			fileprovider.NewFactory(),
			envprovider.NewFactory(),
		},
		ConverterFactories: []confmap.ConverterFactory{},
		DefaultScheme:      "env",
	}

	resolver, err := confmap.NewResolver(resolverSettings)
	if err != nil {
		return config.Supervisor{}, err
	}

	conf, err := resolver.Resolve(context.Background())
	if err != nil {
		return config.Supervisor{}, err
	}

	cfg := config.DefaultSupervisor()
	if err := conf.Unmarshal(&cfg); err != nil {
		return config.Supervisor{}, err
	}

	if err := forceSelfExecutable(&cfg); err != nil {
		return config.Supervisor{}, err
	}

	if err := cfg.Validate(); err != nil {
		return config.Supervisor{}, fmt.Errorf("cannot validate supervisor config %s: %w", configPath, err)
	}

	return cfg, nil
}

func forceSelfExecutable(cfg *config.Supervisor) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("could not determine current executable for agent::executable: %w", err)
	}
	if cfg.Agent.Executable != "" && cfg.Agent.Executable != exe {
		fmt.Fprintf(os.Stderr, "warning: ignoring agent.executable %q from supervisor config; forcing the running Alloy binary %q\n", cfg.Agent.Executable, exe)
	}
	cfg.Agent.Executable = exe
	return nil
}
