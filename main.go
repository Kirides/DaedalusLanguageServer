package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"langsrv/langserver"

	"net/http"
	// _ "net/http/pprof"

	"go.lsp.dev/jsonrpc2"
)

func main() {
	pprofPort := flag.Int("pprof", -1, "enables pprof on the specified port")
	flag.Parse()
	if *pprofPort > 0 {
		go func() {
			http.ListenAndServe(fmt.Sprintf("127.0.0.1:%d", *pprofPort), nil)
		}()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill, syscall.SIGTERM)
	defer stop()

	lspHandler := langserver.NewLspHandler()
	connectLanguageServer(os.Stdin, os.Stdout, lspHandler.TextDocumentSyncHandler, lspHandler).
		Run(ctx)
}

func connectLanguageServer(in io.Reader, out io.Writer, handlers ...jsonrpc2.Handler) *jsonrpc2.Conn {
	bufStream := jsonrpc2.NewStream(in, out)
	rootConn := jsonrpc2.NewConn(bufStream)

	for _, h := range handlers {
		rootConn.AddHandler(h)
	}
	return rootConn
}
