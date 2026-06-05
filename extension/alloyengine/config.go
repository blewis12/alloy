package alloyengine

import (
	"fmt"
	"os"

	"github.com/grafana/alloy/internal/service/remotecfg"
	"github.com/grafana/alloy/syntax/ast"
	"github.com/grafana/alloy/syntax/parser"
)

type Config struct {
	AlloyConfig AlloyConfig       `mapstructure:"config"`
	Flags       map[string]string `mapstructure:"flags"`
}

// This type represents the incoming format of the Alloy configuration
// This is a one-of type, and it is expected that only one of the fields will be set (ie, we cannot define multiple config sources of different types)
type AlloyConfig struct {
	File string `mapstructure:"file"`
}

func (cfg *Config) flagsAsSlice() []string {
	flags := []string{}
	for k, v := range cfg.Flags {
		flags = append(flags, fmt.Sprintf("--%s=%s", k, v))
	}
	return flags
}

func (cfg *Config) Validate() error {
	if cfg.AlloyConfig.File == "" {
		return fmt.Errorf("config.file is required")
	}

	_, err := os.Stat(cfg.AlloyConfig.File)
	if err != nil {
		return fmt.Errorf("provided config path %s does not exist or is not readable: %w", cfg.AlloyConfig.File, err)
	}

	content, err := os.ReadFile(cfg.AlloyConfig.File)
	if err != nil {
		return fmt.Errorf("failed to read config file %s: %w", cfg.AlloyConfig.File, err)
	}

	file, err := parser.ParseFile(cfg.AlloyConfig.File, content)
	if err != nil {
		return fmt.Errorf("failed to parse config file %s: %w", cfg.AlloyConfig.File, err)
	}

	for _, stmt := range file.Body {
		block, ok := stmt.(*ast.BlockStmt)
		if !ok {
			continue
		}

		if block.GetBlockName() == remotecfg.ServiceName {
			return fmt.Errorf("config.file %s contains unsupported %q block for alloyengine", cfg.AlloyConfig.File, remotecfg.ServiceName)
		}
	}

	return nil
}
