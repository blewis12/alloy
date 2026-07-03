package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor"
	"github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/config"
	supervisortelemetry "github.com/open-telemetry/opentelemetry-collector-contrib/cmd/opampsupervisor/supervisor/telemetry"

	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/provider/fileprovider"
)

const (
	// envFleetManagementURL is the base Fleet Management URL (no path); "/v1/opamp" is appended.
	envFleetManagementURL = "GCLOUD_FM_URL"
	// envBasicAuth is the pre-encoded Basic credential, base64(instance_id:token).
	envBasicAuth = "GCLOUD_BASIC_AUTH"
	// envStorageDir optionally overrides the supervisor storage directory (defaults below).
	envStorageDir = "STORAGE_DIR"
)

const defaultStorageDir = "/var/lib/alloy/supervisor"

func registerOpAMPSupervisorCommand(root *cobra.Command) {
	var configPath string

	cmd := &cobra.Command{
		Use:          "otel-supervisor",
		Short:        "Run the embedded OpAMP supervisor for Alloy's OTel engine",
		Long:         "[EXPERIMENTAL] Run an embedded OpAMP supervisor that manages alloy as a supervised agent",
		SilenceUsage: true,
		RunE: func(_ *cobra.Command, _ []string) error {
			return runOpAMPSupervisor(configPath)
		},
	}

	// --config is optional. When omitted, simple mode builds the config from environment variables.
	cmd.Flags().StringVar(&configPath, "config", "", "path to a supervisor configuration file; if omitted, simple mode builds the config from environment variables")

	root.AddCommand(cmd)
}

func runOpAMPSupervisor(configPath string) error {
	var (
		cfg config.Supervisor
		err error
	)
	if configPath == "" {
		// Simple mode: no file, build the config in memory from Grafana Cloud defaults + env vars.
		cfg, err = buildSupervisorConfigFromEnv()
	} else {
		// Manual mode: the file is authoritative.
		cfg, err = loadSupervisorConfig(configPath)
	}
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

func buildSupervisorConfigFromEnv() (config.Supervisor, error) {
	fmURL := strings.TrimSpace(os.Getenv(envFleetManagementURL))
	basicAuth := normalizeBasicAuthCredential(os.Getenv(envBasicAuth))

	var missing []string
	if fmURL == "" {
		missing = append(missing, envFleetManagementURL)
	}
	if basicAuth == "" {
		missing = append(missing, envBasicAuth)
	}
	if len(missing) > 0 {
		return config.Supervisor{}, fmt.Errorf(
			"simple mode requires the environment variable(s) %s to be set (or pass --config <file> to use manual mode)",
			strings.Join(missing, ", "))
	}

	storageDir := strings.TrimSpace(os.Getenv(envStorageDir))
	if storageDir == "" {
		storageDir = defaultStorageDir
	}

	endpoint := fleetManagementEndpoint(fmURL)

	settings := map[string]any{
		"server": map[string]any{
			"endpoint": endpoint,
			"headers": map[string]any{
				"Authorization": "Basic " + basicAuth,
			},
		},
		"capabilities": map[string]any{
			"accepts_remote_config": true,
			"reports_remote_config": true,
		},
		"agent": map[string]any{
			"args":                      []any{"otel"},
			"passthrough_logs":          true,
			"orphan_detection_interval": "5s",
		},
		"storage": map[string]any{
			"directory": storageDir,
		},
		"telemetry": map[string]any{
			"logs": map[string]any{
				"level": "info",
			},
		},
	}

	cfg := config.DefaultSupervisor()
	if err := confmap.NewFromStringMap(settings).Unmarshal(&cfg); err != nil {
		return config.Supervisor{}, fmt.Errorf("failed to build in-memory supervisor config: %w", err)
	}

	if err := forceSelfExecutable(&cfg); err != nil {
		return config.Supervisor{}, err
	}

	if err := cfg.Validate(); err != nil {
		return config.Supervisor{}, fmt.Errorf("cannot validate in-memory supervisor config: %w", err)
	}

	return cfg, nil
}

func fleetManagementEndpoint(fmURL string) string {
	base := strings.TrimRight(strings.TrimSpace(fmURL), "/")
	if strings.HasSuffix(base, "/v1/opamp") {
		return base
	}
	return base + "/v1/opamp"
}

func normalizeBasicAuthCredential(v string) string {
	return strings.Join(strings.Fields(v), "")
}

func loadSupervisorConfig(configPath string) (config.Supervisor, error) {
	if configPath == "" {
		return config.Supervisor{}, fmt.Errorf("path to supervisor configuration file cannot be empty")
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
