package main

import (
	"context"
	"fmt"
)

// The read-only commands land in the next commit; until then they say so.

type catalogOpts struct {
	Kind, Region           string
	Families, Types, Score []string
	Refresh                bool
}

var errNotYet = fmt.Errorf("not implemented yet")

func (e *env) statusJSON(ctx context.Context, name string) error { return errNotYet }
func (e *env) preflight(ctx context.Context, j bool) error       { return errNotYet }
func (e *env) catalog(ctx context.Context, o catalogOpts) error  { return errNotYet }
