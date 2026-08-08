package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/pgeske/filmstream/internal/config"
	"github.com/pgeske/filmstream/internal/indexer"
)

func runIndexer(args []string) error {
	if len(args) == 0 {
		printIndexerUsage()
		return nil
	}
	switch args[0] {
	case "add":
		return runIndexerAdd(args[1:])
	case "list":
		return runIndexerList(args[1:])
	case "test":
		return runIndexerTest(args[1:])
	case "remove":
		return runIndexerRemove(args[1:])
	case "help", "--help", "-h":
		printIndexerUsage()
		return nil
	default:
		return fmt.Errorf("unknown indexer command %q", args[0])
	}
}

func runIndexerAdd(args []string) error {
	flags := flag.NewFlagSet("indexer add", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	name := flags.String("name", "", "unique indexer name")
	noAPIKey := flags.Bool("no-api-key", false, "register a public endpoint without an API key")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *name == "" || len(flags.Args()) != 1 {
		return errors.New("usage: filmstream indexer add --name NAME URL")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	for _, configured := range cfg.Indexers {
		if configured.Name == *name {
			return fmt.Errorf("indexer %q already exists", *name)
		}
	}

	apiKey := ""
	if !*noAPIKey {
		apiKey = os.Getenv("FILMSTREAM_INDEXER_API_KEY")
		if apiKey == "" {
			apiKey, err = promptAPIKey()
			if err != nil {
				return err
			}
		}
	}
	endpoint := flags.Arg(0)
	configured, err := indexer.NewTorznab(*name, endpoint, apiKey, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capabilities, err := configured.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("validate Torznab endpoint: %w", err)
	}

	cfg.Indexers = append(cfg.Indexers, config.Indexer{
		Name:     *name,
		Type:     "torznab",
		Endpoint: endpoint,
		APIKey:   apiKey,
	})
	if err := config.Save(*configPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Added Torznab indexer %q (movie search: %s).\n", *name, yesNo(capabilities.MovieSearchAvailable))
	reportReload(cfg)
	return nil
}

func runIndexerList(args []string) error {
	flags := flag.NewFlagSet("indexer list", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("usage: filmstream indexer list")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	fmt.Printf("%-24s %-18s %s\n", "NAME", "TYPE", "ENDPOINT")
	for _, configured := range cfg.Indexers {
		fmt.Printf("%-24s %-18s %s\n", configured.Name, configured.Type, configured.Endpoint)
	}
	return nil
}

func runIndexerTest(args []string) error {
	flags := flag.NewFlagSet("indexer test", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return errors.New("usage: filmstream indexer test NAME")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	configured, ok := findIndexer(cfg.Indexers, flags.Arg(0))
	if !ok {
		return fmt.Errorf("indexer %q not found", flags.Arg(0))
	}
	if configured.Type != "torznab" {
		fmt.Printf("Indexer %q is built in and does not expose Torznab capabilities.\n", configured.Name)
		return nil
	}
	torznab, err := indexer.NewTorznab(configured.Name, configured.Endpoint, configured.APIKey, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	capabilities, err := torznab.Capabilities(ctx)
	if err != nil {
		return err
	}
	fmt.Printf("Indexer %q is reachable (basic search: %s, movie search: %s).\n",
		configured.Name,
		yesNo(capabilities.SearchAvailable),
		yesNo(capabilities.MovieSearchAvailable),
	)
	return nil
}

func runIndexerRemove(args []string) error {
	flags := flag.NewFlagSet("indexer remove", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 1 {
		return errors.New("usage: filmstream indexer remove NAME")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	name := flags.Arg(0)
	filtered := cfg.Indexers[:0]
	found := false
	for _, configured := range cfg.Indexers {
		if configured.Name == name {
			found = true
			continue
		}
		filtered = append(filtered, configured)
	}
	if !found {
		return fmt.Errorf("indexer %q not found", name)
	}
	cfg.Indexers = filtered
	if err := config.Save(*configPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Removed indexer %q.\n", name)
	reportReload(cfg)
	return nil
}

func reportReload(cfg config.Config) {
	baseURL := "http://" + cfg.Listen
	if !healthy(baseURL) {
		fmt.Println("The change will be used the next time the filmstream server starts.")
		return
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/indexers/reload", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: saved the change but could not build a reload request:", err)
		return
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: saved the change but could not reload the running server:", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fmt.Fprintln(os.Stderr, "Warning: saved the change but the running server rejected the reload:", response.Status)
		return
	}
	fmt.Println("Reloaded the running filmstream server.")
}

func promptAPIKey() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("stdin is not a terminal; set FILMSTREAM_INDEXER_API_KEY or use --no-api-key")
	}
	fmt.Fprint(os.Stderr, "Torznab API key: ")
	value, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", fmt.Errorf("read API key: %w", err)
	}
	apiKey := strings.TrimSpace(string(value))
	if apiKey == "" {
		return "", errors.New("API key cannot be empty; use --no-api-key for a public endpoint")
	}
	return apiKey, nil
}

func findIndexer(indexers []config.Indexer, name string) (config.Indexer, bool) {
	for _, configured := range indexers {
		if configured.Name == name {
			return configured, true
		}
	}
	return config.Indexer{}, false
}

func yesNo(value bool) string {
	if value {
		return "yes"
	}
	return "no"
}

func printIndexerUsage() {
	fmt.Print(`Manage Torznab indexers.

Usage:
  filmstream indexer add --name NAME URL
  filmstream indexer list
  filmstream indexer test NAME
  filmstream indexer remove NAME

The add command prompts for the API key without echoing it. Set
FILMSTREAM_INDEXER_API_KEY for non-interactive use, or pass --no-api-key for a
public endpoint.
`)
}
