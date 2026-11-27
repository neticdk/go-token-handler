// Package cmd is the token-handler command line.
package cmd

import (
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/go-viper/mapstructure/v2"
	"github.com/neticdk/go-token-handler/internal/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

// Execute runs the command and exits non-zero on error.
func Execute() {
	if err := newRootCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newRootCommand() *cobra.Command {
	v := viper.New()
	var cfgFile string

	cmd := &cobra.Command{
		Use:           "token-handler",
		Short:         "Keeps OAuth 2.0 tokens server-side for a single page application",
		Long:          "token-handler is a backend for a single page application. It signs users in, keeps their tokens in a server-side session and proxies API calls with the access token added.",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if cfgFile != "" {
				v.SetConfigFile(cfgFile)
			}
			if err := v.ReadInConfig(); err != nil {
				if _, notFound := err.(viper.ConfigFileNotFoundError); !notFound || cfgFile != "" {
					return fmt.Errorf("read configuration: %w", err)
				}
			}

			logger, err := newLogger(v.GetString("logLevel"))
			if err != nil {
				return err
			}

			var cfg server.Config
			if err := v.Unmarshal(&cfg, viper.DecodeHook(mapstructure.ComposeDecodeHookFunc(
				expandEnv(),
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
			))); err != nil {
				return fmt.Errorf("decode configuration: %w", err)
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return server.Run(ctx, cfg, logger)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&cfgFile, "config", "c", "", "configuration file (default ./config.yaml if present)")
	bind := func(key, env, flag, def, usage string) {
		f.String(flag, def, fmt.Sprintf("%s (%s)", usage, env))
		_ = v.BindPFlag(key, f.Lookup(flag))
		_ = v.BindEnv(key, env)
	}
	bind("logLevel", "LOG_LEVEL", "log-level", "info", "log level: debug, info, warn or error")
	bind("listenAddr", "LISTEN_ADDRESS", "address", ":8080", "listen address")
	bind("basePath", "BASE_PATH", "base-path", "", "path prefix the browser reaches the handler at, such as /bff")
	bind("redirectURL", "REDIRECT_URL", "redirect-url", "", "the application's login callback URL")
	bind("postLogoutRedirectURL", "POST_LOGOUT_REDIRECT_URL", "post-logout-redirect-url", "", "where the provider sends the browser after logout")
	bind("hashKey", "HASH_KEY", "hash-key", "", "base64 key, at least 32 bytes, that signs session data")
	bind("blockKey", "BLOCK_KEY", "block-key", "", "base64 key, 32 bytes, that encrypts session data")
	bind("sessionPath", "SESSION_PATH", "session-path", "", "directory for session files")
	bind("sessionMaxAge", "SESSION_MAX_AGE", "session-max-age", (10 * time.Hour).String(), "absolute session lifetime")
	f.StringSlice("origin", nil, "allowed browser origin, repeatable (ORIGINS, comma separated)")
	_ = v.BindPFlag("origins", f.Lookup("origin"))
	_ = v.BindEnv("origins", "ORIGINS")

	v.SetConfigName("config")
	v.SetConfigType("yaml")
	v.AddConfigPath(".")
	return cmd
}

func newLogger(level string) (*slog.Logger, error) {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: l})), nil
}

// expandEnv replaces ${VAR} in configuration strings, so secrets such as a
// client secret can come from the environment.
func expandEnv() mapstructure.DecodeHookFuncKind {
	return func(f, t reflect.Kind, data any) (any, error) {
		if f != reflect.String || t != reflect.String {
			return data, nil
		}
		s, _ := data.(string)
		if !strings.Contains(s, "$") {
			return s, nil
		}
		return os.ExpandEnv(s), nil
	}
}
