//go:build linux

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// adminInvite is `v2-server admin-invite`: it writes one owner invitation link straight into the
// database and prints it, without starting the server (A6 D4). Whoever can run it on the Cloud
// machine can already change the database and configuration, so it widens nobody's access. It
// gives a new deployment its first owner, and later recovers one (for example when the only owner
// has left).
func adminInvite(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("admin-invite", flag.ContinueOnError)
	fs.SetOutput(out)
	dbURL := fs.String("db", envOr("V2_DATABASE_URL", defaultDatabase), "PostgreSQL URL")
	team := fs.String("team", "", "workspace to invite into; created when missing, required unless exactly one exists")
	if e := fs.Parse(args); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("admin-invite takes no arguments, got %q", fs.Args())
	}
	// Its first run fixes the database's profile, so V2_LOGIN must be the server's (required, as
	// for a server start).
	methods, e := parseLogin(os.Getenv("V2_LOGIN"), false)
	if e != nil {
		return e
	}
	publicURL, e := parsePublicURL(os.Getenv("V2_PUBLIC_URL"))
	if e != nil {
		return e
	}
	name := strings.TrimSpace(*team)
	ctx := context.Background()
	db, e := store.Open(ctx, *dbURL)
	if e != nil {
		return e
	}
	defer db.Close()
	// Run before the first server start, this is what fixes the database's profile.
	if e := db.ClaimProfile(ctx, databaseProfile(methods)); e != nil {
		return e
	}
	if name == "" {
		names, e := db.TenantNames(ctx)
		if e != nil {
			return e
		}
		switch len(names) {
		case 0:
			return errors.New("no workspace exists yet; name the one to create with -team")
		case 1:
			name = names[0]
		default:
			return fmt.Errorf("several workspaces exist (%s); pick one with -team", strings.Join(names, ", "))
		}
	}
	_, token, e := db.CreateOwnerInvite(ctx, name)
	if e != nil {
		return e
	}
	link := "/invite/" + token
	if publicURL != nil {
		link = publicURL.String() + link
	}
	fmt.Fprintf(out, "Owner invitation for %q, usable once within %s:\n%s\n", name, store.OwnerInviteTTL, link)
	if publicURL == nil {
		fmt.Fprintln(out, "V2_PUBLIC_URL is not set: open the path above on the address browsers use for this server.")
	}
	return nil
}
