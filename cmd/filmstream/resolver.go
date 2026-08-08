package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/pgeske/filmstream/internal/config"
	movieresolver "github.com/pgeske/filmstream/internal/resolver"
)

func runResolver(args []string) error {
	if len(args) == 0 {
		printResolverUsage()
		return nil
	}
	switch args[0] {
	case "configure":
		return runResolverConfigure(args[1:])
	case "test":
		return runResolverTest(args[1:])
	case "disable":
		return runResolverDisable(args[1:])
	case "help", "--help", "-h":
		printResolverUsage()
		return nil
	default:
		return fmt.Errorf("unknown resolver command %q", args[0])
	}
}

func runResolverConfigure(args []string) error {
	flags := flag.NewFlagSet("resolver configure", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	baseURL := flags.String("base-url", "https://api.openai.com/v1", "OpenAI-compatible API base URL")
	model := flags.String("model", "gpt-5-nano", "model name")
	apiKeyFile := flags.String("api-key-file", "", "file containing the API key")
	apiKeyEnv := flags.String("api-key-env", "", "environment variable containing the API key")
	noAPIKey := flags.Bool("no-api-key", false, "configure a self-hosted endpoint without authentication")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("usage: filmstream resolver configure [options]")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *apiKeyFile != "" {
		*apiKeyFile, err = filepath.Abs(*apiKeyFile)
		if err != nil {
			return err
		}
	}
	if !*noAPIKey && *apiKeyFile == "" && *apiKeyEnv == "" {
		secret := os.Getenv("FILMSTREAM_RESOLVER_API_KEY")
		if secret == "" {
			secret, err = promptSecret("Model API key")
			if err != nil {
				return err
			}
		}
		*apiKeyFile = filepath.Join(filepath.Dir(config.Path(*configPath)), "resolver-api-key")
		if err := writeSecretFile(*apiKeyFile, secret); err != nil {
			return err
		}
	}

	resolverConfig := config.Resolver{
		Provider: "openai-compatible", BaseURL: *baseURL, Model: *model,
		APIKeyFile: *apiKeyFile, APIKeyEnv: *apiKeyEnv, TimeoutSeconds: 60,
	}
	configured, err := movieresolver.UncachedFromConfig(resolverConfig)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := configured.Resolve(ctx, "the first lord of the rings movie")
	if err != nil {
		return fmt.Errorf("test resolver: %w", err)
	}
	cfg.Resolver = resolverConfig
	if err := config.Save(*configPath, cfg); err != nil {
		return err
	}
	fmt.Printf("Configured %s with model %q. Test resolution: %s.\n", resolverConfig.Provider, resolverConfig.Model, formatResolvedCandidate(result.Candidates[0]))
	reportResolverReload(cfg)
	return nil
}

func runResolverTest(args []string) error {
	flags := flag.NewFlagSet("resolver test", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	configured, err := movieresolver.FromConfig(cfg.Resolver, cfg.DataDir)
	if err != nil {
		return err
	}
	if configured == nil {
		return errors.New("movie resolver is disabled")
	}
	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" {
		query = "the first lord of the rings movie"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result, err := configured.Resolve(ctx, query)
	if err != nil {
		return err
	}
	printResolution(result)
	return nil
}

func runResolverDisable(args []string) error {
	flags := flag.NewFlagSet("resolver disable", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(flags.Args()) != 0 {
		return errors.New("usage: filmstream resolver disable")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	cfg.Resolver = config.Resolver{}
	if err := config.Save(*configPath, cfg); err != nil {
		return err
	}
	fmt.Println("Disabled natural-language movie resolution.")
	reportResolverReload(cfg)
	return nil
}

func runResolve(args []string) error {
	flags := flag.NewFlagSet("resolve", flag.ContinueOnError)
	configPath := flags.String("config", os.Getenv("FILMSTREAM_CONFIG"), "path to config file")
	serverURL := flags.String("server", os.Getenv("FILMSTREAM_SERVER"), "server URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(flags.Args(), " "))
	if query == "" {
		return errors.New("usage: filmstream resolve MOVIE DESCRIPTION")
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}
	if *serverURL == "" {
		*serverURL = "http://" + cfg.Listen
	}
	if err := ensureServer(*serverURL, *configPath, cfg.DataDir, true); err != nil {
		return err
	}
	var result movieresolver.Result
	if err := postJSON(*serverURL+"/v1/resolve", map[string]string{"query": query}, &result); err != nil {
		return err
	}
	printResolution(result)
	return nil
}

func selectResolvedMovie(result movieresolver.Result) (movieresolver.Candidate, error) {
	if len(result.Candidates) == 0 {
		return movieresolver.Candidate{}, errors.New("resolver returned no movie candidates")
	}
	top := result.Candidates[0]
	if top.Confidence >= 0.75 && (len(result.Candidates) == 1 || top.Confidence-result.Candidates[1].Confidence >= 0.1) {
		return top, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		if top.Confidence >= 0.5 {
			return top, nil
		}
		return movieresolver.Candidate{}, errors.New("movie resolution is ambiguous; run 'filmstream resolve' to inspect candidates")
	}

	fmt.Fprintln(os.Stderr, "Which movie did you mean?")
	for i, candidate := range result.Candidates {
		fmt.Fprintf(os.Stderr, "  %d. %s\n", i+1, formatResolvedCandidate(candidate))
	}
	fmt.Fprint(os.Stderr, "Selection: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return movieresolver.Candidate{}, err
	}
	selection, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || selection < 1 || selection > len(result.Candidates) {
		return movieresolver.Candidate{}, errors.New("invalid movie selection")
	}
	return result.Candidates[selection-1], nil
}

func printResolution(result movieresolver.Result) {
	cacheLabel := ""
	if result.Cached {
		cacheLabel = " (cached)"
	}
	fmt.Printf("Input: %s%s\n", result.Input, cacheLabel)
	for i, candidate := range result.Candidates {
		fmt.Printf("%d. %s\n", i+1, formatResolvedCandidate(candidate))
	}
}

func formatResolvedCandidate(candidate movieresolver.Candidate) string {
	year := ""
	if candidate.Year > 0 {
		year = fmt.Sprintf(" (%d)", candidate.Year)
	}
	return fmt.Sprintf("%s%s [%.0f%%]", candidate.Title, year, candidate.Confidence*100)
}

func reportResolverReload(cfg config.Config) {
	baseURL := "http://" + cfg.Listen
	if !healthy(baseURL) {
		fmt.Println("The resolver configuration will be used the next time the filmstream server starts.")
		return
	}
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/resolver/reload", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: saved the resolver but could not build a reload request:", err)
		return
	}
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(request)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Warning: saved the resolver but could not reload the running server:", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		fmt.Fprintln(os.Stderr, "Warning: saved the resolver but the running server rejected the reload:", response.Status)
		return
	}
	fmt.Println("Reloaded the running filmstream server.")
}

func writeSecretFile(path, value string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".resolver-key-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(strings.TrimSpace(value) + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func printResolverUsage() {
	fmt.Print(`Configure natural-language movie resolution through an OpenAI-compatible model.

Usage:
  filmstream resolver configure [--base-url URL] [--model MODEL]
  filmstream resolver test [MOVIE DESCRIPTION]
  filmstream resolver disable
  filmstream resolve MOVIE DESCRIPTION

OpenAI, Ollama, vLLM, llama.cpp, LM Studio, and similar servers can share the
openai-compatible provider. API keys can come from --api-key-file,
--api-key-env, or a secure interactive prompt. Use --no-api-key for a local
server that does not require authentication.
`)
}
