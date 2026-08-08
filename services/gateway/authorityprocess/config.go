package authorityprocess

import (
	"encoding/hex"
	"errors"
	"path/filepath"
)

var errInvalidConfig = errors.New("local authority command: startup rejected")

type commandConfig struct {
	bootstrapPath   string
	bootstrapSHA256 string
}

func parseConfig(arguments []string) (commandConfig, error) {
	if len(arguments) != 4 {
		return commandConfig{}, errInvalidConfig
	}
	var config commandConfig
	for index := 0; index < len(arguments); index += 2 {
		name, value := arguments[index], arguments[index+1]
		switch name {
		case "--bootstrap":
			if config.bootstrapPath != "" || !filepath.IsAbs(value) || filepath.Clean(value) != value {
				return commandConfig{}, errInvalidConfig
			}
			config.bootstrapPath = value
		case "--bootstrap-sha256":
			if config.bootstrapSHA256 != "" || !validLowerSHA256(value) {
				return commandConfig{}, errInvalidConfig
			}
			config.bootstrapSHA256 = value
		default:
			return commandConfig{}, errInvalidConfig
		}
	}
	if config.bootstrapPath == "" || config.bootstrapSHA256 == "" {
		return commandConfig{}, errInvalidConfig
	}
	return config, nil
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func staticStartupError(err error) error {
	if err == nil {
		return nil
	}
	return errInvalidConfig
}
