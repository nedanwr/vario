// Command vario collects AI interactions from local applications into ClickHouse.
package main

import (
	"os"

	"github.com/nedanwr/vario/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
