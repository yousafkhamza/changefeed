package cli

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/signal"

	"github.com/yousafkhamza/changefeed/internal/mcp"
	"github.com/yousafkhamza/changefeed/internal/version"
)

type apiClient struct{ c clientFlags }

func (a apiClient) Get(ctx context.Context, path string, out any) error {
	return do(ctx, a.c, http.MethodGet, path, nil, out)
}

// mcpServe runs the MCP server on stdin/stdout. Logs must never go to stdout,
// which carries the protocol.
func mcpServe(args []string, stdin io.Reader, stdout io.Writer) error {
	fs := newFlags("mcp")
	var c clientFlags
	c.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := c.resolve(); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return mcp.New(apiClient{c}, version.Version, stdout).Serve(ctx, stdin)
}
