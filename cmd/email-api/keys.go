package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nathanblatter/email-api/internal/auth"
	"github.com/nathanblatter/email-api/internal/config"
)

// keysCLI implements:
//
//	email-api keygen <name>   mint a key for a consumer; prints the secret once
//	email-api keys            list keys (never secrets)
//	email-api revoke <name>   revoke a key (its OAuth tokens stop resolving too)
//
// Run inside the container: docker compose exec app email-api keygen natebot
func keysCLI(cfg config.Config, args []string) int {
	usage := func() int {
		fmt.Fprintln(os.Stderr, "usage: email-api keygen <name> | keys | revoke <name>")
		return 2
	}
	if cfg.DatabaseURL == "" {
		fmt.Fprintln(os.Stderr, "DATABASE_URL is not set; named keys need the database")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "db:", err)
		return 1
	}
	defer pool.Close()
	ks, err := auth.OpenPG(ctx, pool, cfg.APIKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch args[0] {
	case "keygen":
		if len(args) != 2 {
			return usage()
		}
		secret, err := ks.Create(ctx, args[1])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Println(secret)
		return 0
	case "keys":
		list, err := ks.List(ctx)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "NAME\tCREATED\tLAST USED\tSTATUS")
		fmt.Fprintf(tw, "env\t(EMAIL_API_KEY)\t\tactive\n")
		for _, k := range list {
			last := "never"
			if k.LastUsed != nil {
				last = k.LastUsed.Local().Format("2006-01-02 15:04")
			}
			status := "active"
			if k.Revoked {
				status = "revoked"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", k.Name, k.CreatedAt.Local().Format("2006-01-02"), last, status)
		}
		return flush(tw)
	case "revoke":
		if len(args) != 2 {
			return usage()
		}
		if err := ks.Revoke(ctx, args[1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Printf("revoked %s\n", strings.TrimSpace(args[1]))
		return 0
	}
	return usage()
}

func flush(tw *tabwriter.Writer) int {
	if err := tw.Flush(); err != nil {
		return 1
	}
	return 0
}
