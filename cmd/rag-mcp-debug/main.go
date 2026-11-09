// Command rag-mcp-debug runs the built-in mock retrieval orchestrator and prints
// the evidence an Agent would receive, without Qdrant or model services.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Guan0923/xinghe_rag/internal/retrieval"
)

func main() { os.Exit(run()) }

func run() int {
	flags := flag.NewFlagSet("rag-mcp-debug", flag.ContinueOnError)
	query := flags.String("query", "RRF 怎么融合 BM25 和 Dense？", "query to run against the mock retriever")
	limit := flags.Int("limit", 5, "maximum number of mock results")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if *query == "" || *limit < 1 {
		fmt.Fprintln(os.Stderr, "rag-mcp-debug: --query must be non-empty and --limit must be positive")
		return 2
	}
	results, err := retrieval.NewMockRetriever().Search(context.Background(), *query, *limit, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "rag-mcp-debug: %v\n", err)
		return 1
	}
	fmt.Printf("mock evidence mode=mock query=%q results=%d\n", *query, len(results))
	for index, result := range results {
		fmt.Printf("%d. %+v\n", index+1, result)
	}
	return 0
}
