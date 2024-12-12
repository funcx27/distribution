package registry

import (
	"fmt"
	"os"
	"time"

	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/distribution/v3/registry/handlers"
	"github.com/distribution/distribution/v3/registry/storage"
	"github.com/distribution/distribution/v3/registry/storage/driver/factory"
	"github.com/distribution/distribution/v3/version"
	"github.com/spf13/cobra"
)

var showVersion bool

func init() {
	RootCmd.AddCommand(ServeCmd)
	RootCmd.AddCommand(GCCmd)
	GCCmd.Flags().StringVarP(&project, "project", "p", "", "project path for cleaning")
	GCCmd.Flags().IntVarP(&retainImageNums, "retain-tags", "", 0, "retain tags per-image")
	GCCmd.Flags().BoolVarP(&dryRun, "dry-run", "d", false, "do everything except remove the blobs")
	GCCmd.Flags().BoolVarP(&removeUntagged, "delete-untagged", "m", false, "delete manifests that are not currently referenced via tag")
	RootCmd.Flags().BoolVarP(&showVersion, "version", "v", false, "show the version and exit")
}

// RootCmd is the main command for the 'registry' binary.
var RootCmd = &cobra.Command{
	Use:   "registry",
	Short: "`registry`",
	Long:  "`registry`",
	Run: func(cmd *cobra.Command, args []string) {
		if showVersion {
			version.PrintVersion()
			return
		}
		// nolint:errcheck
		cmd.Usage()
	},
}

var (
	dryRun          bool
	retainImageNums int
	removeUntagged  bool
	project         string
)

// GCCmd is the cobra command that corresponds to the garbage-collect subcommand
var GCCmd = &cobra.Command{
	Use:   "garbage-collect <config>",
	Short: "`garbage-collect` deletes layers not referenced by any manifests",
	Long:  "`garbage-collect` deletes layers not referenced by any manifests",
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) == 0 {
			args = append(args, "/etc/distribution/config.yml")
		}
		config, err := resolveConfiguration(args)
		if err != nil {
			fmt.Fprintf(os.Stderr, "configuration error: %v\n", err)
			// nolint:errcheck
			cmd.Usage()
			os.Exit(1)
		}

		ctx := dcontext.Background()
		ctx, err = configureLogging(ctx, config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "unable to configure logging with config: %s", err)
			os.Exit(1)
		}

		if retainImageNums > 0 && config != nil {
			_, err := handlers.CleanImages(config, project, retainImageNums, time.Hour*12, dryRun)
			if err != nil {
				os.Exit(1)
			}
		}

		driver, err := factory.Create(ctx, config.Storage.Type(), config.Storage.Parameters())
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to construct %s driver: %v", config.Storage.Type(), err)
			os.Exit(1)
		}

		registry, err := storage.NewRegistry(ctx, driver)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to construct registry: %v", err)
			os.Exit(1)
		}

		err = storage.MarkAndSweep(ctx, driver, registry, storage.GCOpts{
			DryRun:         dryRun,
			RemoveUntagged: removeUntagged,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to garbage collect: %v", err)
			os.Exit(1)
		}
	},
}
