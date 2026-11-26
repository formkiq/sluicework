package main

import (
	"errors"
	"os"
	"strings"

	"sluicework/internal/envconfig"
)

// A coding workspace must never select an environment file implicitly.
func environmentArgs(args []string) ([]string, error) {
	if len(args) == 0 {
		return args, nil
	}
	var path string
	switch {
	case args[0] == "--env-file":
		if len(args) < 3 {
			return nil, errors.New("use --env-file FILE before a Sluicework command")
		}
		path, args = args[1], args[2:]
	case strings.HasPrefix(args[0], "--env-file="):
		path, args = strings.TrimPrefix(args[0], "--env-file="), args[1:]
	default:
		return args, nil
	}
	if path == "" || len(args) == 0 || strings.HasPrefix(args[0], "--env-file") {
		return nil, errors.New("select one --env-file FILE before a Sluicework command")
	}
	values, err := envconfig.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return nil, errors.New("cannot apply environment file")
			}
		}
	}
	return args, nil
}
