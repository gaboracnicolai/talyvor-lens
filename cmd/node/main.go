package main

// talyvor-node — mining binary GPU operators run to contribute
// inference capacity to the Lens network. Commands:
//   start        register with Lens, serve inference, heartbeat
//   stop         deregister + wipe local state
//   status       print the local state + a live health probe
//   models       enumerate models from the configured provider
//   earnings     pretty-print LENS earnings + balance
//
// Separate binary so operators don't need to ship the whole
// Lens proxy + its dependencies on every GPU box.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// heartbeatInterval matches the spec — 30s between pings.
const heartbeatInterval = 30 * time.Second

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(1)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "start":
		runStart(args)
	case "stop":
		runStop(args)
	case "status":
		runStatus(args)
	case "models":
		runModels(args)
	case "earnings":
		runEarnings(args)
	case "-h", "--help", "help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		printUsage()
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Fprint(os.Stderr, `talyvor-node — mining binary for the Lens network

Usage:
  talyvor-node <command>

Commands:
  start      Register with Lens and start serving inference
  stop       Deregister and wipe local state
  status     Show node status + live health
  models     List models reported by the local provider
  earnings   Show LENS earnings + balance

Configuration (env vars):
  LENS_URL              Lens server URL
  LENS_API_KEY          API key to authenticate with Lens
  LENS_WORKSPACE_ID     Workspace this node belongs to
  NODE_URL              Public URL Lens uses to reach this node
  NODE_PROVIDER         ollama | vllm | llamacpp (default: ollama)
  NODE_MODELS           comma-separated models (required)
  NODE_GPU_TYPE         cpu | rtx4090 | a100 | h100 (default: cpu)
  NODE_PORT             listen port (default: 9090)
  NODE_MAX_CONCURRENT   max parallel inferences (default: 4)
  NODE_PROVIDER_URL     override the local provider endpoint
`)
}

// ─── start ───────────────────────────────────────

func runStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	_ = fs.Parse(args)

	cfg := LoadConfig()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("❌ config error: %v", err)
	}

	provider, err := NewProvider(cfg.Provider, cfg.ProviderURL())
	if err != nil {
		log.Fatalf("❌ provider: %v", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if err := provider.Health(probeCtx); err != nil {
		cancel()
		log.Fatalf("❌ local %s provider not reachable at %s: %v", cfg.Provider, cfg.ProviderURL(), err)
	}
	cancel()

	client := NewLensClient(cfg.LensURL, cfg.LensAPIKey)
	regCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	state, err := client.Register(regCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("❌ register with Lens: %v", err)
	}

	log.Printf("📡 Registered with Lens at %s", cfg.LensURL)
	log.Printf("🔧 Serving models: %s", strings.Join(cfg.Models, ", "))
	log.Printf("🪪 Node ID: %s", state.NodeID)

	srv := NewInferenceServer(provider, state.NodeSecret, cfg)
	httpServer, _ := srv.ListenAndServe(cfg.Port)

	// Heartbeat loop — runs until ctx is cancelled by the
	// signal handler below.
	hbCtx, hbCancel := context.WithCancel(context.Background())
	defer hbCancel()
	go heartbeatLoop(hbCtx, client, state, srv, provider)

	// Block until SIGTERM/SIGINT.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Printf("🛑 shutdown signal received — deregistering")

	hbCancel()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if err := client.Deregister(shutdownCtx, state); err != nil {
		log.Printf("⚠️  deregister failed: %v", err)
	}
	log.Printf("👋 bye")
}

// heartbeatLoop pings Lens every 30s with the live counters.
// Failures are logged but never fatal — Lens marks us inactive
// after 90s of silence so a transient blip recovers naturally.
func heartbeatLoop(ctx context.Context, client *LensClient, state NodeState, srv *InferenceServer, p Provider) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			beatCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			models, _ := p.ListModels(beatCtx)
			if err := client.Heartbeat(beatCtx, state,
				srv.ActiveRequests(), srv.UptimeSeconds(), models); err != nil {
				log.Printf("⚠️  heartbeat failed: %v", err)
			}
			cancel()
		}
	}
}

// ─── stop ────────────────────────────────────────

func runStop(_ []string) {
	state, err := LoadState()
	if err != nil {
		log.Fatalf("❌ load state: %v", err)
	}
	if state.NodeID == "" {
		log.Printf("⚠️  no registered node found; nothing to do")
		return
	}
	apiKey := os.Getenv("LENS_API_KEY")
	if apiKey == "" {
		log.Fatalf("❌ LENS_API_KEY required to deregister")
	}
	client := NewLensClient(state.LensURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := client.Deregister(ctx, state); err != nil {
		log.Fatalf("❌ deregister: %v", err)
	}
	log.Printf("👋 deregistered node %s", state.NodeID)
}

// ─── status ──────────────────────────────────────

func runStatus(_ []string) {
	state, err := LoadState()
	if err != nil || state.NodeID == "" {
		log.Fatalf("❌ no registered node found — run `talyvor-node start` first")
	}
	cfg := LoadConfig()
	provider, err := NewProvider(state.Provider, cfg.ProviderURL())
	if err != nil {
		log.Fatalf("❌ provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	healthy := provider.Health(ctx) == nil
	fmt.Print(FormatStatus(state, healthy))
}

// ─── models ──────────────────────────────────────

func runModels(_ []string) {
	cfg := LoadConfig()
	if cfg.Provider == "" {
		cfg.Provider = "ollama"
	}
	provider, err := NewProvider(cfg.Provider, cfg.ProviderURL())
	if err != nil {
		log.Fatalf("❌ provider: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	models, err := provider.ListModels(ctx)
	if err != nil {
		log.Fatalf("❌ list models: %v", err)
	}
	if len(models) == 0 {
		fmt.Println("(no models reported by provider)")
		return
	}
	for _, m := range models {
		fmt.Println(m)
	}
}

// ─── earnings ────────────────────────────────────

func runEarnings(_ []string) {
	state, err := LoadState()
	if err != nil || state.NodeID == "" {
		log.Fatalf("❌ no registered node found — run `talyvor-node start` first")
	}
	apiKey := os.Getenv("LENS_API_KEY")
	if apiKey == "" {
		log.Fatalf("❌ LENS_API_KEY required to query earnings")
	}
	client := NewLensClient(state.LensURL, apiKey)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	earnings, err := client.Earnings(ctx, state.WorkspaceID)
	if err != nil {
		log.Fatalf("❌ earnings: %v", err)
	}
	balance, err := client.Balance(ctx, state.WorkspaceID)
	if err != nil {
		// Balance is optional — render with what we have.
		log.Printf("⚠️  balance lookup failed: %v", err)
		balance = map[string]any{}
	}
	_ = errors.Is // keep stdlib referenced if we later add error-class checks
	fmt.Print(FormatEarnings(state, earnings, balance))
}
