package main

import (
	"fmt"
	"os"

	"github.com/brimdata/zq/cmd/zed/zst"
	_ "github.com/brimdata/zq/cmd/zed/zst/create"
	_ "github.com/brimdata/zq/cmd/zed/zst/cut"
	_ "github.com/brimdata/zq/cmd/zed/zst/inspect"
	_ "github.com/brimdata/zq/cmd/zed/zst/read"
	"github.com/brimdata/zq/pkg/charm"
)

func main() {
	zst.Cmd.Add(charm.Help)
	if _, err := zst.Cmd.ExecRoot(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
}