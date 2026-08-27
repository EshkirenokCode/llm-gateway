// Command gwctl manages tenants and API keys. It talks to the database
// directly: the gateway itself deliberately exposes no administrative HTTP
// surface, so a leaked tenant key can never create another one.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/EshkirenokCode/llm-gateway/internal/auth"
	"github.com/EshkirenokCode/llm-gateway/internal/billing"
	"github.com/EshkirenokCode/llm-gateway/internal/storage"
	"github.com/EshkirenokCode/llm-gateway/internal/storage/db"
)

const usage = `gwctl manages gateway tenants and keys.

Usage:
  gwctl tenant create -name NAME -prefix PREFIX [-budget USD]
  gwctl tenant list
  gwctl tenant budget -id UUID -budget USD
  gwctl key create -tenant UUID [-name NAME] [-rpm N]
  gwctl key list -tenant UUID
  gwctl key revoke -id UUID
  gwctl usage -tenant UUID [-days N]

The database connection comes from DATABASE_URL.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("no command given")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("DATABASE_URL is not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	store, err := storage.Open(ctx, dsn, 2)
	if err != nil {
		return err
	}
	defer store.Close()

	switch args[0] {
	case "tenant":
		return tenantCommand(ctx, store, args[1:])
	case "key":
		return keyCommand(ctx, store, args[1:])
	case "usage":
		return usageCommand(ctx, store, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func tenantCommand(ctx context.Context, store *storage.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("tenant needs a subcommand: create, list or budget")
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("tenant create", flag.ExitOnError)
		name := fs.String("name", "", "display name")
		prefix := fs.String("prefix", "", "short marker embedded in the tenant keys")
		budget := fs.Float64("budget", 0, "monthly budget in US dollars, 0 for unlimited")
		soft := fs.Float64("soft-limit", 0.8, "fraction of the budget that triggers a warning")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		if *name == "" || *prefix == "" {
			return errors.New("both -name and -prefix are required")
		}

		params := db.CreateTenantParams{
			Name:           *name,
			Prefix:         *prefix,
			SoftLimitRatio: numeric(*soft),
		}
		if *budget > 0 {
			micros := int64(*budget * 1_000_000)
			params.BudgetMicros = &micros
		}

		tenant, err := store.CreateTenant(ctx, params)
		if err != nil {
			return fmt.Errorf("create tenant: %w", err)
		}

		fmt.Printf("tenant %s created\n", tenant.ID)
		return nil

	case "list":
		tenants, err := store.ListTenants(ctx)
		if err != nil {
			return fmt.Errorf("list tenants: %w", err)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tPREFIX\tBUDGET")
		for _, t := range tenants {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Prefix, budgetText(t.BudgetMicros))
		}
		return w.Flush()

	case "budget":
		fs := flag.NewFlagSet("tenant budget", flag.ExitOnError)
		id := fs.String("id", "", "tenant id")
		budget := fs.Float64("budget", 0, "monthly budget in US dollars, 0 for unlimited")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		tenantID, err := uuid.Parse(*id)
		if err != nil {
			return fmt.Errorf("invalid tenant id: %w", err)
		}

		params := db.SetTenantBudgetParams{ID: tenantID}
		if *budget > 0 {
			micros := int64(*budget * 1_000_000)
			params.BudgetMicros = &micros
		}

		tenant, err := store.SetTenantBudget(ctx, params)
		if err != nil {
			return fmt.Errorf("set budget: %w", err)
		}

		fmt.Printf("tenant %s budget is now %s\n", tenant.ID, budgetText(tenant.BudgetMicros))
		return nil

	default:
		return fmt.Errorf("unknown tenant subcommand %q", args[0])
	}
}

func keyCommand(ctx context.Context, store *storage.Store, args []string) error {
	if len(args) == 0 {
		return errors.New("key needs a subcommand: create, list or revoke")
	}

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("key create", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id")
		name := fs.String("name", "", "what this key is for")
		rpm := fs.Int("rpm", 0, "requests per minute, 0 for unlimited")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		tenantID, err := uuid.Parse(*tenant)
		if err != nil {
			return fmt.Errorf("invalid tenant id: %w", err)
		}

		row, err := store.GetTenant(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("look up tenant: %w", err)
		}

		key, err := auth.Generate(row.Prefix)
		if err != nil {
			return err
		}

		params := db.CreateAPIKeyParams{
			TenantID: tenantID,
			KeyHash:  key.Hash,
			LastFour: key.LastFour,
			Name:     *name,
		}
		if *rpm > 0 {
			limit := int32(*rpm)
			params.RateLimitRpm = &limit
		}

		created, err := store.CreateAPIKey(ctx, params)
		if err != nil {
			return fmt.Errorf("create api key: %w", err)
		}

		// This is the only time the plaintext exists outside the client.
		fmt.Printf("key %s created for tenant %s\n\n  %s\n\nStore it now, it cannot be shown again.\n",
			created.ID, row.Name, key.Plaintext)
		return nil

	case "list":
		fs := flag.NewFlagSet("key list", flag.ExitOnError)
		tenant := fs.String("tenant", "", "tenant id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		tenantID, err := uuid.Parse(*tenant)
		if err != nil {
			return fmt.Errorf("invalid tenant id: %w", err)
		}

		keys, err := store.ListAPIKeys(ctx, tenantID)
		if err != nil {
			return fmt.Errorf("list keys: %w", err)
		}

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tNAME\tLAST4\tRPM\tREVOKED\tLAST USED")
		for _, k := range keys {
			rpm := "-"
			if k.RateLimitRpm != nil {
				rpm = strconv.Itoa(int(*k.RateLimitRpm))
			}
			revoked := "no"
			if k.RevokedAt.Valid {
				revoked = k.RevokedAt.Time.Format(time.RFC3339)
			}
			lastUsed := "never"
			if k.LastUsedAt.Valid {
				lastUsed = k.LastUsedAt.Time.Format(time.RFC3339)
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", k.ID, k.Name, k.LastFour, rpm, revoked, lastUsed)
		}
		return w.Flush()

	case "revoke":
		fs := flag.NewFlagSet("key revoke", flag.ExitOnError)
		id := fs.String("id", "", "key id")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}

		keyID, err := uuid.Parse(*id)
		if err != nil {
			return fmt.Errorf("invalid key id: %w", err)
		}

		if err := store.RevokeAPIKey(ctx, keyID); err != nil {
			return fmt.Errorf("revoke key: %w", err)
		}

		fmt.Printf("key %s revoked; running gateways honour it within their key cache ttl\n", keyID)
		return nil

	default:
		return fmt.Errorf("unknown key subcommand %q", args[0])
	}
}

func usageCommand(ctx context.Context, store *storage.Store, args []string) error {
	fs := flag.NewFlagSet("usage", flag.ExitOnError)
	tenant := fs.String("tenant", "", "tenant id")
	days := fs.Int("days", 30, "how far back to look")
	if err := fs.Parse(args); err != nil {
		return err
	}

	tenantID, err := uuid.Parse(*tenant)
	if err != nil {
		return fmt.Errorf("invalid tenant id: %w", err)
	}

	since := time.Now().AddDate(0, 0, -*days)

	spent, err := store.SpentSince(ctx, tenantID, since)
	if err != nil {
		return err
	}

	rows, err := store.ListTenantRequests(ctx, db.ListTenantRequestsParams{
		TenantID:  tenantID,
		CreatedAt: timestamp(since),
		Limit:     20,
	})
	if err != nil {
		return fmt.Errorf("list requests: %w", err)
	}

	fmt.Printf("spent in the last %d days: %s\n\n", *days, spent)

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "WHEN\tMODEL\tSTATUS\tPROMPT\tCOMPLETION\tCOST")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n",
			r.CreatedAt.Time.Format(time.RFC3339), r.Model, r.StatusCode,
			r.PromptTokens, r.CompletionTokens, billing.Micros(r.CostMicros))
	}
	return w.Flush()
}

func budgetText(micros *int64) string {
	if micros == nil {
		return "unlimited"
	}
	return billing.Micros(*micros).String()
}

func numeric(v float64) pgtype.Numeric {
	var n pgtype.Numeric
	if err := n.Scan(strconv.FormatFloat(v, 'f', 3, 64)); err != nil {
		return pgtype.Numeric{}
	}
	return n
}

func timestamp(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
