// Command parity runs parity scenarios against the Swift and Go servers.
//
//	parity compare --swift URL --swift-db DSN --go URL --go-db DSN --scenarios DIR [--module M] [--save-swift-transcripts DIR]
//	parity smoke   --target URL --token JWT --transcripts DIR --scenarios DIR --module M
//
// compare exits 1 on any difference. A self-test is compare with --go pointing
// at a second Swift instance: zero diff proves the scenarios are deterministic.
// smoke (gate G2) runs `smoke: true` scenarios against a deployed server and
// compares status codes with the Swift transcripts saved by CI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"lumina/internal/auth"
	"lumina/internal/config"
	"lumina/parity/scenario"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: parity compare|smoke [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "compare":
		err = compare(os.Args[2:])
	case "smoke":
		err = smoke(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "parity:", err)
		os.Exit(1)
	}
}

func compare(args []string) error {
	fs := flag.NewFlagSet("compare", flag.ExitOnError)
	swiftURL := fs.String("swift", "http://localhost:18080", "Swift base URL")
	swiftDB := fs.String("swift-db", "", "Swift database DSN")
	goURL := fs.String("go", "http://localhost:18081", "Go base URL (a second Swift for self-test)")
	goDB := fs.String("go-db", "", "Go database DSN")
	secrets := fs.String("jwt-secrets", "parity:parity-secret-parity-secret-0000", "JWT_HMAC_SECRETS shared by both servers")
	users := fs.String("users", "parity/seed/users.yaml", "fixture users")
	dir := fs.String("scenarios", "parity/scenarios", "scenario root")
	module := fs.String("module", "", "only this module (default: all)")
	save := fs.String("save-swift-transcripts", "", "write Swift transcripts here")
	_ = fs.Parse(args)

	ctx := context.Background()
	keys, err := auth.LoadKeys(config.Config{JWTHMACSecrets: *secrets})
	if err != nil {
		return err
	}
	fixture, err := loadUsers(*users)
	if err != nil {
		return err
	}
	swiftT, err := target(ctx, "swift", *swiftURL, *swiftDB, keys, fixture)
	if err != nil {
		return err
	}
	goT, err := target(ctx, "go", *goURL, *goDB, keys, fixture)
	if err != nil {
		return err
	}
	scenarios, err := loadAll(*dir, *module)
	if err != nil {
		return err
	}

	var diffs []scenario.Difference
	for _, sc := range scenarios {
		a, err := scenario.Run(ctx, sc, swiftT)
		if err != nil {
			return err
		}
		b, err := scenario.Run(ctx, sc, goT)
		if err != nil {
			return err
		}
		if *save != "" {
			if err := saveTranscript(*save, sc, a); err != nil {
				return err
			}
		}
		diffs = append(diffs, scenario.Diff(a, b)...)
	}
	fmt.Printf("%d scenarios, %d differences\n", len(scenarios), len(diffs))
	for _, d := range diffs {
		fmt.Printf("  %s / %s  %s\n    swift: %s\n    go:    %s\n", d.Scenario, d.Step, d.Field, d.Swift, d.Go)
	}
	if len(diffs) > 0 {
		return errors.New("parity failed")
	}
	return nil
}

func smoke(args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	url := fs.String("target", "http://localhost:18090", "deployed server (port-forward)")
	token := fs.String("token", os.Getenv("STAGING_JWT"), "session JWT for the smoke user")
	transcripts := fs.String("transcripts", "swift-transcripts", "Swift transcripts saved by CI")
	dir := fs.String("scenarios", "parity/scenarios", "scenario root")
	module := fs.String("module", "", "module to smoke-test")
	_ = fs.Parse(args)
	if *token == "" || *module == "" {
		return errors.New("smoke needs --token (or STAGING_JWT) and --module")
	}
	scenarios, err := loadAll(*dir, *module)
	if err != nil {
		return err
	}
	failed := 0
	ran := 0
	for _, sc := range scenarios {
		if !sc.Smoke {
			continue
		}
		ran++
		raw, err := os.ReadFile(filepath.Join(*transcripts, sc.Module, sc.File+".json"))
		if err != nil {
			return fmt.Errorf("no saved Swift transcript for %s: %w", sc.File, err)
		}
		var want scenario.Transcript
		if err := json.Unmarshal(raw, &want); err != nil {
			return err
		}
		got, err := scenario.Run(context.Background(), sc, scenario.Target{Name: "staging", BaseURL: *url, Token: *token})
		if err != nil {
			return err
		}
		for i, st := range got.Steps {
			if i >= len(want.Steps) || want.Steps[i].Status != st.Status {
				failed++
				fmt.Printf("  %s / %s: status %d, Swift transcript has %v\n", sc.File, st.Name, st.Status, statusAt(want, i))
			}
		}
	}
	fmt.Printf("%d smoke scenarios, %d status mismatches\n", ran, failed)
	if ran == 0 {
		return fmt.Errorf("module %s has no smoke: true scenarios", *module)
	}
	if failed > 0 {
		return errors.New("smoke failed")
	}
	return nil
}

func statusAt(t scenario.Transcript, i int) any {
	if i < len(t.Steps) {
		return t.Steps[i].Status
	}
	return "<no step>"
}

func target(ctx context.Context, name, url, dsn string, keys auth.Keyset, users map[string]uuid.UUID) (scenario.Target, error) {
	t := scenario.Target{Name: name, BaseURL: url, Keys: keys, Users: users}
	if dsn != "" {
		pool, err := pgxpool.New(ctx, dsn)
		if err != nil {
			return t, err
		}
		t.Snapshot = scenario.PGSnapshotter(pool)
	}
	return t, nil
}

func loadUsers(path string) (map[string]uuid.UUID, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	out := make(map[string]uuid.UUID, len(m))
	for name, id := range m {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, fmt.Errorf("user %s: %w", name, err)
		}
		out[name] = u
	}
	return out, nil
}

// loadAll reads <root>/<module>/*.yaml for one module, or every module dir.
func loadAll(root, module string) ([]scenario.Scenario, error) {
	dirs := []string{filepath.Join(root, module)}
	if module == "" {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		dirs = dirs[:0]
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(root, e.Name()))
			}
		}
	}
	var out []scenario.Scenario
	for _, d := range dirs {
		scs, err := scenario.Load(d)
		if err != nil {
			return nil, err
		}
		out = append(out, scs...)
	}
	return out, nil
}

func saveTranscript(root string, sc scenario.Scenario, t scenario.Transcript) error {
	dir := filepath.Join(root, sc.Module)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	b, err := t.JSON()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, sc.File+".json"), b, 0o600)
}
