package cmd

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/chrishrb/go-grip/internal"
	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:     "go-grip [file]",
	Short:   "Render markdown document as html",
	Version: buildVersion(),
	Args:    cobra.MatchAll(cobra.OnlyValidArgs),
	PreRunE: func(cmd *cobra.Command, args []string) error {
		_, err := logLevel(cmd)
		return err
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// avoid printing the usage text on a bad host or a busy port
		cmd.SilenceUsage = true

		browser, _ := cmd.Flags().GetBool("browser")
		host, _ := cmd.Flags().GetString("host")
		port, _ := cmd.Flags().GetInt("port")
		boundingBox, _ := cmd.Flags().GetBool("bounding-box")
		disabledFeatures, _ := cmd.Flags().GetStringSlice("disable-markdown-feature")
		noReload, _ := cmd.Flags().GetBool("no-reload")
		closeTab, _ := cmd.Flags().GetBool("close-tab-on-exit")
		replaceHistory, _ := cmd.Flags().GetBool("replace-history")
		findPort, _ := cmd.Flags().GetBool("find-port")
		logger, err := newLogger(cmd, os.Stderr)
		if err != nil {
			return err
		}

		var file string
		if len(args) == 1 {
			file = args[0]
		}

		if err := internal.ValidateMarkdownFeatures(disabledFeatures); err != nil {
			return err
		}

		parser := internal.NewParser(disabledFeatures)
		server := internal.NewServer(host, port, boundingBox, browser, !noReload, parser, logger)
		server.SetCloseTabs(closeTab)
		server.SetReplaceNav(replaceHistory)
		server.SetFindPort(findPort)
		return server.Serve(file)
	},
}

// logLevel returns the validated --log-level value.
func logLevel(cmd *cobra.Command) (slog.Level, error) {
	value, _ := cmd.Flags().GetString("log-level")
	level, err := internal.ParseLogLevel(value)
	if err != nil {
		return 0, fmt.Errorf("--log-level: %w", err)
	}
	return level, nil
}

// newLogger builds the one logger the server and its watcher share.
func newLogger(cmd *cobra.Command, w io.Writer) (*slog.Logger, error) {
	level, err := logLevel(cmd)
	if err != nil {
		return nil, err
	}
	return internal.NewLogger(w, level), nil
}

func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	rootCmd.SetVersionTemplate("{{.Name}} {{.Version}}\n")
	rootCmd.Flags().BoolP("browser", "b", true, "Open new browser tab")
	rootCmd.Flags().StringP("host", "H", "localhost", "Host or IP to listen on")
	rootCmd.Flags().IntP("port", "p", 6419, "Port to use")
	rootCmd.Flags().Bool("find-port", false, "If the port is busy, use the next free one (tries up to 100 ports)")
	rootCmd.Flags().Bool("bounding-box", true, "Add bounding box to HTML")
	rootCmd.Flags().StringSlice(
		"disable-markdown-feature",
		nil,
		"Disable optional markdown feature(s): "+strings.Join(internal.SupportedMarkdownFeatures(), ", "),
	)
	rootCmd.Flags().Bool("no-reload", false, "Disable automatic browser reload on file changes")
	rootCmd.Flags().Bool("close-tab-on-exit", true, "Close the preview tab when the server stops (only tabs the browser lets scripts close)")
	rootCmd.Flags().Bool("replace-history", false, "Follow links inside the preview without adding browser history entries, so --close-tab-on-exit still works after navigating (disables back/forward inside the preview)")
	rootCmd.Flags().String("log-level", "info", "Log level on stderr: error, warn, info or debug")
}
