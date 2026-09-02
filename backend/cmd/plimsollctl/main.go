// Command plimsollctl is the operator CLI. It connects as plimsoll_owner and is run by
// hand, exactly like goose -- it is never reachable over HTTP.
//
// Minting invites is an administrative act, so it lives here rather than behind an
// endpoint. Giving the request-serving role a path to create accounts would put an admin
// backdoor in the process most exposed to the internet, for no gain: there is no invite
// UI in V1 (K16).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Contictus/plimsoll/backend/internal/auth"
	"github.com/Contictus/plimsoll/backend/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "plimsollctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	const usage = "usage: plimsollctl invite -email <address> [-ttl 168h]"

	// The subcommand is split off before flag parsing: flag.Parse stops at the first
	// non-flag argument, so passing it the whole slice would silently ignore every flag
	// that follows "invite".
	if len(args) == 0 || args[0] != "invite" {
		return fmt.Errorf("%s", usage)
	}

	fs := flag.NewFlagSet("invite", flag.ContinueOnError)
	email := fs.String("email", "", "email address the invite is bound to")
	ttl := fs.Duration("ttl", 7*24*time.Hour, "how long the invite stays valid")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *email == "" {
		return fmt.Errorf("%s", usage)
	}

	dsn := os.Getenv("PLIMSOLL_OWNER_DSN")
	if dsn == "" {
		return fmt.Errorf("PLIMSOLL_OWNER_DSN is not set")
	}

	ctx := context.Background()
	pool, err := store.NewPool(ctx, dsn)
	if err != nil {
		return err
	}
	defer pool.Close()

	plain, hash, err := auth.NewOpaqueToken()
	if err != nil {
		return err
	}
	expires := time.Now().Add(*ttl)
	if err := store.New(pool).CreateInvite(ctx, store.CreateInviteParams{
		TokenHash: hash,
		Email:     *email,
		ExpiresAt: expires,
	}); err != nil {
		return fmt.Errorf("insert invite for %s: %w", *email, err)
	}

	// Printed once, to a human, on purpose: only the hash is stored, so this is the only
	// moment the token exists in readable form. It is never logged (L13).
	fmt.Printf("invite for %s\nexpires %s\ntoken   %s\n",
		*email, expires.Format(time.RFC3339), plain)
	return nil
}
