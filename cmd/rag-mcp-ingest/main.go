// Command rag-mcp-ingest is the local-directory ingest CLI. The Python command
// depends on the full ingest runtime (Qdrant + model API embedding pipeline),
// which is not yet wired in Go; this skeleton documents the limitation and
// exits 2 so scripts do not silently succeed.
package main

import (
	"flag"
	"fmt"
	"os"
)

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("rag-mcp-ingest", flag.ContinueOnError)
	_ = flags.String("dir", "", "local directory to ingest")
	_ = flags.String("namespace", "", "target namespace")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	fmt.Fprintln(os.Stderr, "rag-mcp-ingest: not yet ported to Go; local-directory ingest requires the "+
		"runtime ingest pipeline (Qdrant index publication + model API embedding), which is still a TODO. "+
		"Use the documented upload/MCP path instead.")
	return 2
}
