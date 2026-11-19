//go:build !cshared

// Command cpa-quota is the CLIProxyAPI plugin library. Build it with
//
//	go build -tags cshared -buildmode=c-shared -o cpa-quota.so ./cmd/cpa-quota
//
// Without the cshared tag this stub only reports that.
package main

import (
	"fmt"
	"os"

	"github.com/sketchain/cpa-quota/internal/plugin"
)

func main() {
	fmt.Fprintf(os.Stderr, "%s %s is a CLIProxyAPI plugin library; build it with -tags cshared -buildmode=c-shared\n", plugin.PluginID, plugin.Version)
	os.Exit(2)
}
