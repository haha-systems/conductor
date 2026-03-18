package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/haha-systems/conductor/internal/config"
	"github.com/haha-systems/conductor/internal/domain"
	"github.com/haha-systems/conductor/internal/proof"
	"github.com/haha-systems/conductor/internal/provider"
	"github.com/haha-systems/conductor/internal/router"
	"github.com/haha-systems/conductor/internal/supervisor"
	"github.com/haha-systems/conductor/internal/worksource"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	var cfgPath string

	root := &cobra.Command{
		Use:   "conductor",
		Short: "Multi-provider coding agent orchestrator",
	}
	root.PersistentFlags().StringVar(&cfgPath, "config", "conductor.toml", "path to conductor.toml")

	root.AddCommand(
		runCmd(&cfgPath),
		collectProofCmd(&cfgPath),
		landCmd(&cfgPath),
		costCmd(&cfgPath),
	)
	return root
}

// runCmd starts the polling loop.
func runCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Start polling for tasks and running agents",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*cfgPath)
			if err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGINT, syscall.SIGTERM)
			defer stop()

			return startOrchestrator(ctx, cfg)
		},
	}
}

// collectProofCmd runs proof collection for a completed run.
func collectProofCmd(cfgPath *string) *cobra.Command {
	var runID string

	cmd := &cobra.Command{
		Use:   "collect-proof",
		Short: "Collect proof artifacts for a completed run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*cfgPath)
			if err != nil {
				return err
			}

			worktreePath := filepath.Join(cfg.Sandbox.WorktreeDir, runID)
			summaryPath := filepath.Join(worktreePath, "proof", "summary.json")

			data, err := os.ReadFile(summaryPath)
			if err != nil {
				return fmt.Errorf("run %s: proof not found at %s: %w", runID, summaryPath, err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "run ID (required)")
	cmd.MarkFlagRequired("run-id") //nolint:errcheck
	return cmd
}

// landCmd safely lands a reviewed run.
func landCmd(cfgPath *string) *cobra.Command {
	var runID string

	cmd := &cobra.Command{
		Use:   "land",
		Short: "Rebase, re-run CI, and merge a reviewed run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*cfgPath)
			if err != nil {
				return err
			}

			worktreePath := filepath.Join(cfg.Sandbox.WorktreeDir, runID)
			lander := proof.NewLander(proof.Config{
				RequireCIPass: true,
				PRBaseBranch:  cfg.Proof.PRBaseBranch,
			})

			sha, err := lander.Land(cmd.Context(), worktreePath)
			if err != nil {
				return fmt.Errorf("land failed: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Landed at %s\n", sha)
			return nil
		},
	}
	cmd.Flags().StringVar(&runID, "run-id", "", "run ID to land (required)")
	cmd.MarkFlagRequired("run-id") //nolint:errcheck
	return cmd
}

// costCmd prints per-run cost information from proof/summary.json files.
func costCmd(cfgPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "cost",
		Short: "Show cost summary for completed runs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(*cfgPath)
			if err != nil {
				return err
			}

			entries, err := os.ReadDir(cfg.Sandbox.WorktreeDir)
			if err != nil {
				return fmt.Errorf("reading worktree dir %s: %w", cfg.Sandbox.WorktreeDir, err)
			}

			var total float64
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%-30s %-12s %-10s %s\n", "RUN ID", "PROVIDER", "COST (USD)", "TASK")
			fmt.Fprintf(w, "%s\n", "------------------------------------------------------------")

			for _, entry := range entries {
				if !entry.IsDir() {
					continue
				}
				summaryPath := filepath.Join(cfg.Sandbox.WorktreeDir, entry.Name(), "proof", "summary.json")
				data, err := os.ReadFile(summaryPath)
				if err != nil {
					continue // run may not have a proof yet
				}
				var bundle domain.ProofBundle
				if err := json.Unmarshal(data, &bundle); err != nil {
					continue
				}
				fmt.Fprintf(w, "%-30s %-12s $%-9.4f %s\n",
					bundle.RunID, bundle.Provider, bundle.CostUSD, bundle.TaskID)
				total += bundle.CostUSD
			}

			fmt.Fprintf(w, "%s\n", "------------------------------------------------------------")
			fmt.Fprintf(w, "%-30s %-12s $%.4f\n", "TOTAL", "", total)
			return nil
		},
	}
}

// startOrchestrator wires together all components and runs the main loop.
func startOrchestrator(ctx context.Context, cfg *config.Config) error {
	slog.Info("conductor starting",
		"max_concurrent_runs", cfg.Conductor.MaxConcurrentRuns,
		"default_provider", cfg.Conductor.DefaultProvider,
	)

	// Build enabled providers.
	providers := buildProviders(cfg)
	if len(providers) == 0 {
		return fmt.Errorf("no enabled providers configured")
	}

	// Build work source.
	source, err := buildWorkSource(cfg)
	if err != nil {
		return err
	}

	// Wire components.
	rt := router.NewWithPersonas(
		providers,
		cfg.Routing.LabelRoutes,
		cfg.Routing.PersonaRoutes,
		cfg.Personas,
		cfg.Routing.Strategy,
		cfg.Conductor.DefaultProvider,
	)

	sup := supervisor.New(supervisor.Config{
		WorktreeBaseDir:   cfg.Sandbox.WorktreeDir,
		TimeoutMinutes:    cfg.Sandbox.TimeoutMinutes,
		PreserveOnFailure: cfg.Sandbox.PreserveOnFailure,
		RepoRoot:          repoRoot(),
		WorkflowFile:      cfg.Sandbox.WorkflowFile,
	})

	proofCollector := proof.New(proof.Config{
		RequireCIPass: cfg.Proof.RequireCIPass,
		PRBaseBranch:  cfg.Proof.PRBaseBranch,
	})

	poller := worksource.NewPoller(source, worksource.PollerConfig{
		IntervalSeconds:   cfg.Conductor.WorkIntervalSeconds,
		MaxConcurrentRuns: cfg.Conductor.MaxConcurrentRuns,
	})

	taskCh := poller.Run(ctx)

	for task := range taskCh {
		go func() {
			defer poller.Done()
			executeTask(ctx, task, rt, sup, proofCollector, source, cfg.Hooks)
		}()
	}

	slog.Info("conductor stopped")
	return nil
}

func executeTask(
	ctx context.Context,
	task *domain.Task,
	rt *router.Router,
	sup *supervisor.Supervisor,
	collector *proof.Collector,
	source worksource.WorkSource,
	hooks []string,
) {
	log := slog.With("task_id", task.ID)

	route, err := rt.Route(task)
	if err != nil {
		log.Error("routing failed", "error", err)
		return
	}

	if route.RaceN > 1 {
		executeRace(ctx, task, route, sup, collector, source, hooks, log)
		return
	}

	p := route.Providers[0]
	log.Info("task routed", "provider", p.Name())
	executeRun(ctx, task, p, route.Persona, sup, collector, source, hooks, log)
}

// executeRace spawns N parallel runs and takes the first success.
func executeRace(
	ctx context.Context,
	task *domain.Task,
	route router.RouteResult,
	sup *supervisor.Supervisor,
	collector *proof.Collector,
	source worksource.WorkSource,
	hooks []string,
	log *slog.Logger,
) {
	type outcome struct {
		result *supervisor.Result
		p      provider.AgentProvider
	}

	raceCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan outcome, len(route.Providers))

	for _, p := range route.Providers {
		go func() {
			run := &domain.Run{ID: newRunID(), TaskID: task.ID, Provider: p.Name()}
			result := sup.Execute(raceCtx, supervisor.RunRequest{
				Run:      run,
				Task:     task,
				Provider: p,
				Persona:  route.Persona,
				Source:   source,
			})
			ch <- outcome{result: result, p: p}
		}()
	}

	var winner *supervisor.Result
	var winnerProvider provider.AgentProvider
	failures := 0

	for range len(route.Providers) {
		out := <-ch
		if out.result.Err == nil && winner == nil {
			winner = out.result
			winnerProvider = out.p
			cancel() // context cancellation stops the remaining runs
		} else {
			failures++
		}
	}

	if winner == nil {
		log.Error("all race runs failed", "count", len(route.Providers))
		source.PostResult(ctx, task, fmt.Sprintf("All %d race runs failed", len(route.Providers))) //nolint:errcheck
		return
	}

	log.Info("race winner", "provider", winnerProvider.Name(), "run_id", winner.Run.ID, "failures", failures)
	finishRun(ctx, winner.Run, task, collector, source, hooks, log)
}

func executeRun(
	ctx context.Context,
	task *domain.Task,
	p provider.AgentProvider,
	persona *config.PersonaConfig,
	sup *supervisor.Supervisor,
	collector *proof.Collector,
	source worksource.WorkSource,
	hooks []string,
	log *slog.Logger,
) {
	run := &domain.Run{ID: newRunID(), TaskID: task.ID, Provider: p.Name()}
	result := sup.Execute(ctx, supervisor.RunRequest{
		Run:      run,
		Task:     task,
		Provider: p,
		Persona:  persona,
		Source:   source,
	})
	if result.Err != nil {
		log.Error("run failed", "run_id", run.ID, "error", result.Err)
		if task.Type != domain.TaskTypeRebase {
			source.PostResult(ctx, task, fmt.Sprintf("Run failed: %v", result.Err)) //nolint:errcheck
		}
		return
	}
	if task.Type == domain.TaskTypeRebase {
		return // outcome already recorded by supervisor via RecordRebaseOutcome
	}
	finishRun(ctx, run, task, collector, source, hooks, log)
}

func finishRun(
	ctx context.Context,
	run *domain.Run,
	task *domain.Task,
	collector *proof.Collector,
	source worksource.WorkSource,
	hooks []string,
	log *slog.Logger,
) {
	log.Info("run succeeded", "run_id", run.ID)

	bundle, err := collector.Collect(ctx, run, task)
	if err != nil {
		log.Error("proof collection failed", "run_id", run.ID, "error", err)
		source.PostResult(ctx, task, fmt.Sprintf("Run succeeded but proof collection failed: %v", err)) //nolint:errcheck
		return
	}

	if err := source.PostResult(ctx, task, formatProofSummary(bundle)); err != nil {
		log.Error("post result failed", "error", err)
	}

	// Run post-run hooks with the summary path.
	if len(hooks) > 0 {
		summaryPath := filepath.Join(run.WorktreePath, "proof", "summary.json")
		if err := proof.RunHooks(ctx, hooks, summaryPath); err != nil {
			log.Warn("post-run hook failed", "run_id", run.ID, "error", err)
		}
	}

	log.Info("run complete — worktree preserved", "run_id", run.ID, "path", run.WorktreePath)
}

func formatProofSummary(b *domain.ProofBundle) string {
	data, _ := json.MarshalIndent(b, "", "  ")
	return "```json\n" + string(data) + "\n```"
}

func buildProviders(cfg *config.Config) map[string]provider.AgentProvider {
	providers := make(map[string]provider.AgentProvider)
	for name, pcfg := range cfg.Providers {
		if !pcfg.Enabled {
			continue
		}
		switch name {
		case "claude":
			providers[name] = provider.NewClaudeCodeAdapter(pcfg.Binary, pcfg.ExtraArgs, pcfg.CostPer1kTokens)
		case "codex":
			providers[name] = provider.NewCodexAdapter(pcfg.Binary, pcfg.ExtraArgs)
		case "gemini":
			providers[name] = provider.NewGeminiCLIAdapter(pcfg.Binary, pcfg.ExtraArgs, pcfg.CostPer1kTokens)
		case "opencode":
			providers[name] = provider.NewOpenCodeAdapter(pcfg.Binary, pcfg.ExtraArgs, pcfg.CostPer1kTokens)
		default:
			// Treat unknown names with a binary set as custom adapters.
			if pcfg.Binary != "" {
				providers[name] = provider.NewCustomAdapter(name, pcfg.Binary, pcfg.ExtraArgs, pcfg.CostPer1kTokens)
			} else {
				slog.Warn("unknown provider type with no binary, skipping", "name", name)
			}
		}
	}
	return providers
}

func buildWorkSource(cfg *config.Config) (worksource.WorkSource, error) {
	if cfg.WorkSources.GitHub != nil {
		token := os.Getenv("GITHUB_TOKEN")
		if token == "" {
			return nil, fmt.Errorf("GITHUB_TOKEN env var required for GitHub work source")
		}
		return worksource.NewGitHubSource(token, cfg.WorkSources.GitHub.Repo, cfg.WorkSources.GitHub.LabelFilter)
	}
	if cfg.WorkSources.Linear != nil {
		token := os.Getenv("LINEAR_API_KEY")
		if token == "" {
			return nil, fmt.Errorf("LINEAR_API_KEY env var required for Linear work source")
		}
		return worksource.NewLinearSource(token, cfg.WorkSources.Linear.TeamID, cfg.WorkSources.Linear.StateFilter)
	}
	return nil, fmt.Errorf("no work source configured — add [work_sources.github] or [work_sources.linear] to conductor.toml")
}

func repoRoot() string {
	// Walk up from cwd to find the git root.
	dir, _ := os.Getwd()
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir // fallback
		}
		dir = parent
	}
}

func newRunID() string {
	return fmt.Sprintf("run_%d", time.Now().UnixNano())
}
