//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/agentruntime"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/workspacereclaim"
)

// reclaimsCommand lists the reclaims of deleted conversations' checkouts for operators: kept ones
// with their path and reason, open ones with their attempts and last error, reclaimed ones with
// what they released and the delivery they relied on. It only reads, so it runs beside a server.
func reclaimsCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reclaims", flag.ContinueOnError)
	fs.SetOutput(out)
	dbURL := fs.String("db", envOr("V2_DATABASE_URL", defaultDatabase), "PostgreSQL URL")
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: v2-server reclaims [-db URL] [pending|deleting|kept|reclaimed ...]")
	}
	if e := fs.Parse(args); e != nil {
		return e
	}
	var statuses []store.ReclaimStatus
	for _, a := range fs.Args() {
		switch st := store.ReclaimStatus(a); st {
		case store.ReclaimPending, store.ReclaimDeleting, store.ReclaimKept, store.ReclaimReclaimed:
			statuses = append(statuses, st)
		default:
			return fmt.Errorf("unknown reclaim status %q", a)
		}
	}
	ctx := context.Background()
	db, e := store.Open(ctx, *dbURL)
	if e != nil {
		return e
	}
	defer db.Close()
	list, e := db.WorkspaceReclaims(ctx, statuses...)
	if e != nil {
		return e
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "WORKSPACE\tSTATUS\tMODE\tREASON\tATTEMPTS\tLAST ERROR\tBYTES\tFILES\tDELIVERY\tDELIVERY COMMIT\tBASE COMMIT\tREQUESTED\tPATH")
	for _, r := range list {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%d\t%d\t%s\t%s\t%s\t%s\t%s\n", r.WorkspaceID, r.Status, r.Mode, r.Reason, r.Attempts,
			r.LastError, r.RemovedBytes, r.RemovedFiles, r.DeliveryID, r.DeliveryCommit, r.BaseCommit, r.RequestedAt.Format(time.RFC3339), r.Path)
	}
	return w.Flush()
}

// discardKeptCommand removes one kept checkout for an operator, with the server stopped: it holds
// the data directory lock, checks the path as any reclaim does, and records the reclaim as
// reclaimed by an operator.
func discardKeptCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("discard-kept", flag.ContinueOnError)
	fs.SetOutput(out)
	dbURL := fs.String("db", envOr("V2_DATABASE_URL", defaultDatabase), "PostgreSQL URL")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if fs.NArg() != 1 {
		return errors.New("usage: v2-server discard-kept [-db URL] <workspace id>")
	}
	base, e := dataDirectory()
	if e != nil {
		return e
	}
	lock, e := lockDirectory(base)
	if e != nil {
		return e
	}
	defer lock.Close()
	ctx := context.Background()
	db, e := store.Open(ctx, *dbURL)
	if e != nil {
		return e
	}
	defer db.Close()
	r := workspacereclaim.New(db, nil, agentruntime.OfflineCheckouts(filepath.Join(base, "nodes")))
	o, e := r.DiscardKept(ctx, fs.Arg(0))
	if e != nil {
		return e
	}
	fmt.Fprintf(out, "reclaimed %s: %d bytes in %d entries\n", fs.Arg(0), o.Bytes, o.Files)
	return nil
}
