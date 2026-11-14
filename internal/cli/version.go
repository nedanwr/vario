package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/nedanwr/vario/internal/buildinfo"
)

func (a *app) versionCmd() *cobra.Command {
	var short bool
	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the Vario version",
		Args:  cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			info := buildinfo.Get()
			if short {
				fmt.Fprintln(a.stdout, info.Version)
				return
			}
			fmt.Fprintln(a.stdout, info.String())
		},
	}
	cmd.Flags().BoolVar(&short, "short", false, "print only the version number")
	return cmd
}
