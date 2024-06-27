// Code generated by cmd/lexgen (see Makefile's lexgen); DO NOT EDIT.

package bsky

// schema: app.bsky.graph.getStarterPacks

import (
	"context"

	"github.com/bluesky-social/indigo/xrpc"
)

// GraphGetStarterPacks_Output is the output of a app.bsky.graph.getStarterPacks call.
type GraphGetStarterPacks_Output struct {
	StarterPacks []*GraphDefs_StarterPackViewBasic `json:"starterPacks" cborgen:"starterPacks"`
}

// GraphGetStarterPacks calls the XRPC method "app.bsky.graph.getStarterPacks".
func GraphGetStarterPacks(ctx context.Context, c *xrpc.Client, uris []string) (*GraphGetStarterPacks_Output, error) {
	var out GraphGetStarterPacks_Output

	params := map[string]interface{}{
		"uris": uris,
	}
	if err := c.Do(ctx, xrpc.Query, "", "app.bsky.graph.getStarterPacks", params, nil, &out); err != nil {
		return nil, err
	}

	return &out, nil
}