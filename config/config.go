// Package config provides configuration for taskrunner.
//
// Config is the exported, usable configuration object, built from
// ReadConfig.
//
// Internally, the package represents the json file as a separate struct type,
// configFile. This distinction helps make features like extending configurations
// easier to support and test.
package config

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
)

// configFile is the json representation for the .taskrunner.json
// file format.
type configFile struct {
	configPath string

	Extends      *string  `json:"extends"`
	Path         *string  `json:"path"`
	DesiredTasks []string `json:"desiredTasks"`
}

// resolveWorkingDir resolves the pathOption into an absolute path. It assumes
// pathOption (the "path" field in the config file) is relative to filePath (the config file).
func resolveWorkingDir(filePath, pathOption string) (string, error) {
	configPath, err := filepath.Abs(filePath)
	if err != nil {
		return "", fmt.Errorf("unable to find config file %s: %w", filePath, err)
	}

	return filepath.Join(filepath.Dir(configPath), pathOption), nil
}

// ToConfig converts a configFile representation to a usable Config.
func (c configFile) ToConfig() (*Config, error) {
	config := Config{
		DesiredTasks: c.DesiredTasks,
		ConfigPath:   c.configPath,
	}

	if c.Path == nil {
		return nil, fmt.Errorf("config must specify path: %s", c.configPath)
	}

	resolvedPath, err := resolveWorkingDir(c.configPath, *c.Path)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve path for %s: %w", *c.Path, err)
	}
	config.WorkingDir = resolvedPath

	return &config, nil
}

// Config defines user parameters for taskrunner. It is usually loaded
// via ReadUserConfig or ReadConfig.
type Config struct {
	// WorkingDir is the resolved working directory from the "path" attribute.
	WorkingDir string

	// CaonfigPath is the path to the file from where this config was loaded.
	ConfigPath string

	// DesiredTasks is the set of tasks to run specified by this configuration.
	DesiredTasks []string
}

// With extends the base configuration with settings from the input configFile,
// returning a new config without modifying the original.
func (c *Config) With(configFile *configFile) (*Config, error) {
	copy := *c

	if path := configFile.Path; path != nil {
		resolvedPath, err := resolveWorkingDir(configFile.configPath, *path)
		if err != nil {
			return nil, fmt.Errorf("failed to resolve path for %s: %w", *path, err)
		}
		copy.WorkingDir = resolvedPath
	}

	if configFile.DesiredTasks != nil {
		merged := make(map[string]struct{})
		for _, task := range c.DesiredTasks {
			merged[task] = struct{}{}
		}
		for _, task := range configFile.DesiredTasks {
			merged[task] = struct{}{}
		}

		mergedTasks := make([]string, 0, len(merged))
		for task := range merged {
			mergedTasks = append(mergedTasks, task)
		}
		copy.DesiredTasks = mergedTasks
	}

	copy.ConfigPath = configFile.configPath

	return &copy, nil
}

// readConfigFile reads the configuration at configPath and returns the json file representation.
func readConfigFile(configPath string) (*configFile, error) {
	resolvedConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("unable to resolve config file path: %w", err)
	}

	content, err := ioutil.ReadFile(resolvedConfigPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read file: %w", err)
	}

	configFile := configFile{configPath: resolvedConfigPath}
	if err := json.Unmarshal(content, &configFile); err != nil {
		return nil, fmt.Errorf("unable to parse json: %w", err)
	}

	return &configFile, nil
}

// ReadConfig reads the configuration at configPath and returns a Config.
func ReadConfig(configPath string) (*Config, error) {
	configFile, err := readConfigFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file at %s: %w", configPath, err)
	}

	if configFile.Extends == nil {
		config, err := configFile.ToConfig()
		if err != nil {
			return nil, fmt.Errorf("failed to build config from %s: %w", configFile.configPath, err)
		}
		return config, nil
	}

	extends, err := filepath.Abs(filepath.Join(filepath.Dir(configPath), *configFile.Extends))
	if err != nil {
		return nil, fmt.Errorf("failed to resolve base config at %s: %w", *configFile.Extends, err)
	}

	baseConfig, err := ReadConfig(extends)
	if err != nil {
		return nil, fmt.Errorf("failed to read base config at %s: %w", extends, err)
	}

	merged, err := baseConfig.With(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read base config at %s: %w", extends, err)
	}

	return merged, nil
}

// ReadDefaultConfig reads the default configuration from workspace.taskrunner.json.
// If present, user.taskrunner.json will be used instead.
func ReadDefaultConfig() (*Config, error) {
	workspaceConfig := "./workspace.taskrunner.json"
	userConfig := "./user.taskrunner.json"

	if _, err := os.Stat(userConfig); err == nil {
		return ReadConfig(userConfig)
	}

	return ReadConfig(workspaceConfig)
}
