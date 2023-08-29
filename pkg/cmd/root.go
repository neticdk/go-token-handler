package cmd

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/url"
	"os"
	"reflect"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/mitchellh/mapstructure"
	"github.com/neticdk/go-token-handler/pkg/auth"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"golang.org/x/oauth2"
)

const (
	listenAddr  = "listenAddr"
	hashKey     = "hashKey"
	blockKey    = "blockKey"
	origins     = "origins"
	redirectURL = "redirectURL"
	providers   = "providers"
	upstreams   = "upstreams"
	sessionPath = "sessionPath"
)

type provider struct {
	Issuer       string
	ClientID     string
	ClientSecret string
}

var (
	logLevel int
	cfgFile  string

	rootCmd = &cobra.Command{
		Use:   "token-handler",
		Short: "Handling secure OAuth 2.0 authentication for single page apps",
		Long:  "The token-handler is a utility to act as a server-side secure storage of OAuth 2.0 tokens for single page applications.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if cfgFile != "" {
				viper.SetConfigFile(cfgFile)
			}
			err := viper.ReadInConfig()
			if _, ok := err.(viper.ConfigParseError); ok {
				return err
			}

			e := echo.New()
			e.Debug = true
			e.HideBanner = true
			e.Use(middleware.Logger())

			hashKey, err := base64.StdEncoding.DecodeString(viper.GetString(hashKey))
			if err != nil {
				return err
			}
			blockKey, err := base64.StdEncoding.DecodeString(viper.GetString(blockKey))
			if err != nil {
				return err
			}
			fs := sessions.NewFilesystemStore(viper.GetString(sessionPath), hashKey, blockKey)
			fs.MaxLength(0)
			e.Use(session.Middleware(fs))

			var provs map[string]provider
			err = viper.Sub(providers).Unmarshal(&provs, viper.DecodeHook(StringExpandEnv()))
			if err != nil {
				return err
			}

			ps := map[string]*oauth2.Config{}
			for n, p := range provs {
				provider, err := oidc.NewProvider(context.Background(), p.Issuer)
				if err != nil {
					return err
				}
				ps[n] = &oauth2.Config{
					ClientID:     p.ClientID,
					ClientSecret: p.ClientSecret,
					Endpoint:     provider.Endpoint(),
					RedirectURL:  viper.GetString(redirectURL),
				}
			}

			am := auth.RegisterAuthEndpoint(e, hashKey, blockKey, ps, viper.GetStringSlice(origins))

			var ups map[string]string
			err = viper.Sub(upstreams).Unmarshal(&ups)
			if err != nil {
				return err
			}

			for p, u := range ups {
				g := e.Group(p)
				u, err := url.Parse(u)
				if err != nil {
					return fmt.Errorf("unable to parse url %s for path %s: %w", u, p, err)
				}
				g.Use(
					middleware.CORSWithConfig(middleware.CORSConfig{
						AllowOrigins:     viper.GetStringSlice(origins),
						AllowMethods:     []string{"*"},
						AllowCredentials: true,
					}),
					am,
					middleware.Proxy(middleware.NewRoundRobinBalancer([]*middleware.ProxyTarget{
						{
							URL: u,
						},
					})),
				)
			}

			if err := e.Start(viper.GetString(listenAddr)); err != nil {
				return err
			}

			return nil
		},
	}
)

// Execute root command
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.PersistentFlags().StringVarP(&cfgFile, "config", "c", "", "Configuration file")
	rootCmd.PersistentFlags().IntVarP(&logLevel, "v", "v", 4, "Log level verbosity 0 is only panic level and 6 is trace level")

	rootCmd.Flags().String("address", "", "Listen address (LISTEN_ADDRESS)")
	_ = viper.BindPFlag(listenAddr, rootCmd.Flags().Lookup("address"))
	_ = viper.BindEnv(listenAddr, "LISTEN_ADDRESS")
	viper.SetDefault(listenAddr, ":8080")

	rootCmd.Flags().String("hash-key", "", "Key used to sign cookie content (HASH_KEY)")
	_ = viper.BindPFlag(hashKey, rootCmd.Flags().Lookup("hash-key"))
	_ = viper.BindEnv(hashKey, "HASH_KEY")

	rootCmd.Flags().String("block-key", "", "Key used to encrypt cookie content (BLOCK_KEY)")
	_ = viper.BindPFlag(blockKey, rootCmd.Flags().Lookup("block-key"))
	_ = viper.BindEnv(blockKey, "BLOCK_KEY")

	rootCmd.Flags().StringSliceP("origin", "o", []string{}, "Origins to set for CORS (ORIGINS) - default: http://localhost:3000")
	_ = viper.BindPFlag(origins, rootCmd.Flags().Lookup("origin"))
	_ = viper.BindEnv(origins)
	viper.SetDefault(origins, "http://localhost:3000")

	rootCmd.Flags().String("redirect-url", "", "Redirect URL for authentication (required if multiple are registeret with identity server) (REDIRECT_URL)")
	_ = viper.BindPFlag(redirectURL, rootCmd.Flags().Lookup("redirect-url"))
	_ = viper.BindEnv(redirectURL, "REDIRECT_URL")

	rootCmd.Flags().String("session-path", "", "Path to store encrypted session data (SESSION_PATH)")
	_ = viper.BindPFlag(sessionPath, rootCmd.Flags().Lookup("session-path"))
	_ = viper.BindEnv(sessionPath, "SESSION_PATH")
	viper.SetDefault(sessionPath, "")

	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")
}

func StringExpandEnv() mapstructure.DecodeHookFuncKind {
	return func(f reflect.Kind, t reflect.Kind, data interface{}) (interface{}, error) {
		if f != reflect.String || t != reflect.String {
			return data, nil
		}
		return os.ExpandEnv(data.(string)), nil
	}
}
