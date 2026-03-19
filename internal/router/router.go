package router

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	charmlog "github.com/charmbracelet/log"

	"github.com/haha-systems/conductor/internal/config"
	"github.com/haha-systems/conductor/internal/domain"
	"github.com/haha-systems/conductor/internal/provider"
)

// Router selects a provider (or providers, in race mode) for a given task.
type Router struct {
	providers     map[string]provider.AgentProvider
	labelRoutes   map[string]string
	personaRoutes map[string]string
	personas      map[string]config.PersonaConfig
	strategy      string
	defaultName   string
	roundRobinIdx atomic.Uint64
}

// RouteResult is returned by Route.
type RouteResult struct {
	// Providers contains one entry normally, or N entries in race mode.
	Providers []provider.AgentProvider
	// RaceN is the number of parallel runs requested (1 = normal, >1 = race).
	RaceN int
	// Persona is the resolved persona, or nil if no persona was selected.
	Persona *config.PersonaConfig
}

// New creates a Router.
func New(
	providers map[string]provider.AgentProvider,
	labelRoutes map[string]string,
	strategy string,
	defaultName string,
) *Router {
	return &Router{
		providers:     providers,
		labelRoutes:   labelRoutes,
		personaRoutes: map[string]string{},
		personas:      map[string]config.PersonaConfig{},
		strategy:      strategy,
		defaultName:   defaultName,
	}
}

// NewWithPersonas creates a Router with persona routing support.
func NewWithPersonas(
	providers map[string]provider.AgentProvider,
	labelRoutes map[string]string,
	personaRoutes map[string]string,
	personas map[string]config.PersonaConfig,
	strategy string,
	defaultName string,
) *Router {
	if personaRoutes == nil {
		personaRoutes = map[string]string{}
	}
	if personas == nil {
		personas = map[string]config.PersonaConfig{}
	}
	return &Router{
		providers:     providers,
		labelRoutes:   labelRoutes,
		personaRoutes: personaRoutes,
		personas:      personas,
		strategy:      strategy,
		defaultName:   defaultName,
	}
}

// Route selects providers for the given task.
// Precedence (highest → lowest):
//  1. Pinned agent (front-matter agent:) → provider directly, no persona
//  2. Pinned persona (front-matter persona:) → persona's provider (or default)
//  3. Label-based persona route (persona_routes) → persona's provider
//  4. Label-based provider route (label_routes) → provider directly
//  5. Global strategy → provider
//  6. Default provider
func (r *Router) Route(task *domain.Task) (RouteResult, error) {
	// 1. Pinned via front-matter agent field — no persona.
	if task.Config != nil && task.Config.Agent != "" {
		p, err := r.get(task.Config.Agent)
		if err != nil {
			return RouteResult{}, err
		}
		result := RouteResult{Providers: []provider.AgentProvider{p}, RaceN: 1}
		charmlog.Debug("route resolved", "task_id", task.ID, "provider", p.Name(), "strategy", "pinned_agent")
		return result, nil
	}

	// 2. Routing strategy from front-matter overrides global config.
	strategy := r.strategy
	if task.Config != nil && task.Config.Routing != "" {
		strategy = task.Config.Routing
	}

	// Resolve persona (steps 2–3).
	persona := r.resolvePersona(task)

	if persona != nil {
		// Use the persona's provider if set, otherwise fall back to default.
		providerName := persona.Provider
		if providerName == "" {
			providerName = r.defaultName
		}
		p, err := r.get(providerName)
		if err != nil {
			return RouteResult{}, err
		}
		result := RouteResult{Providers: []provider.AgentProvider{p}, RaceN: 1, Persona: persona}
		charmlog.Debug("route resolved", "task_id", task.ID, "labels", strings.Join(task.Labels, ","), "persona", persona.Name, "provider", p.Name(), "strategy", "persona_route")
		return result, nil
	}

	// 4. Label-based routing (checked before strategy).
	for _, label := range task.Labels {
		if providerName, ok := r.labelRoutes[label]; ok {
			p, err := r.get(providerName)
			if err != nil {
				return RouteResult{}, err
			}
			result := RouteResult{Providers: []provider.AgentProvider{p}, RaceN: 1}
			charmlog.Debug("route resolved", "task_id", task.ID, "labels", strings.Join(task.Labels, ","), "provider", p.Name(), "strategy", "label_route")
			return result, nil
		}
	}

	result, err := r.applyStrategy(strategy)
	if err != nil {
		return RouteResult{}, err
	}
	if len(result.Providers) > 0 {
		charmlog.Debug("route resolved", "task_id", task.ID, "provider", result.Providers[0].Name(), "strategy", strategy)
	}
	return result, nil
}

// resolvePersona returns the persona to use for this task, or nil.
// Checks front-matter persona: first, then label persona_routes.
func (r *Router) resolvePersona(task *domain.Task) *config.PersonaConfig {
	// Front-matter pinned persona.
	if task.Config != nil && task.Config.Persona != "" {
		name := task.Config.Persona
		if p, ok := r.personas[name]; ok {
			return &p
		}
		charmlog.Warn("unknown persona in task front-matter, falling through", "persona", name)
		return nil
	}

	// Label-based persona route — first match wins.
	for _, label := range task.Labels {
		if personaName, ok := r.personaRoutes[label]; ok {
			if p, ok := r.personas[personaName]; ok {
				return &p
			}
			charmlog.Warn("persona_routes references unknown persona, falling through", "persona", personaName, "label", label)
		}
	}

	return nil
}

func (r *Router) applyStrategy(strategy string) (RouteResult, error) {
	lower := strings.ToLower(strings.TrimSpace(strategy))

	switch {
	case lower == "round-robin" || lower == "":
		p, err := r.roundRobin()
		if err != nil {
			return RouteResult{}, err
		}
		return RouteResult{Providers: []provider.AgentProvider{p}, RaceN: 1}, nil

	case lower == "cheapest":
		p, err := r.cheapest()
		if err != nil {
			return RouteResult{}, err
		}
		return RouteResult{Providers: []provider.AgentProvider{p}, RaceN: 1}, nil

	case strings.HasPrefix(lower, "race"):
		return r.raceStrategy(lower)

	default:
		// Unknown strategy — fall back to default provider.
		p, err := r.get(r.defaultName)
		if err != nil {
			return RouteResult{}, err
		}
		return RouteResult{Providers: []provider.AgentProvider{p}, RaceN: 1}, nil
	}
}

func (r *Router) get(name string) (provider.AgentProvider, error) {
	p, ok := r.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %q not found or not enabled", name)
	}
	return p, nil
}

func (r *Router) roundRobin() (provider.AgentProvider, error) {
	names := r.enabledNames()
	if len(names) == 0 {
		return nil, fmt.Errorf("no enabled providers available")
	}
	idx := r.roundRobinIdx.Add(1) - 1
	return r.providers[names[idx%uint64(len(names))]], nil
}

// cheapest selects the provider with the lowest cost estimate for a median-sized
// task (4000 chars ≈ 1000 tokens). Falls back to round-robin if all providers
// return unknown cost.
func (r *Router) cheapest() (provider.AgentProvider, error) {
	names := r.enabledNames()
	if len(names) == 0 {
		return nil, fmt.Errorf("no enabled providers available")
	}

	const sampleLen = 4000
	bestCost := math.MaxFloat64
	var best provider.AgentProvider

	for _, name := range names {
		p := r.providers[name]
		cost, ok := p.CostEstimate(sampleLen)
		if ok && cost < bestCost {
			bestCost = cost
			best = p
		}
	}

	if best == nil {
		// All providers returned unknown — fall back to round-robin.
		return r.roundRobin()
	}
	return best, nil
}

// raceStrategy parses "race N" and returns N providers (cycling through all enabled ones).
// Example: "race 2" returns 2 different providers.
func (r *Router) raceStrategy(strategy string) (RouteResult, error) {
	n := 2 // default if just "race" with no number
	parts := strings.Fields(strategy)
	if len(parts) >= 2 {
		parsed, err := strconv.Atoi(parts[1])
		if err != nil || parsed < 1 {
			return RouteResult{}, fmt.Errorf("invalid race count in %q", strategy)
		}
		n = parsed
	}

	names := r.enabledNames()
	if len(names) == 0 {
		return RouteResult{}, fmt.Errorf("no enabled providers available")
	}
	if n > len(names) {
		n = len(names) // can't race more providers than we have
	}

	// Pick n providers starting from the round-robin cursor.
	idx := int(r.roundRobinIdx.Add(uint64(n)) - uint64(n))
	selected := make([]provider.AgentProvider, n)
	for i := range n {
		selected[i] = r.providers[names[(idx+i)%len(names)]]
	}

	return RouteResult{Providers: selected, RaceN: n}, nil
}

func (r *Router) enabledNames() []string {
	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
